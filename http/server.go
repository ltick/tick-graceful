// Package gracehttp provides easy to use graceful restart
// functionality for HTTP server.
package http

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"log"
	libNet "net"
	libHttp "net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"lianjia/clock"
	"lianjia/net"
)

const (
	DefaultStopTimeout = time.Minute
	DefaultKillTimeout = time.Minute
)

var (
	didInherit = os.Getenv("LISTEN_FDS") != ""
	ppid       = os.Getppid()
)

// An app contains one or more servers and associated configuration.
type Server struct {
	Server      *libHttp.Server
	Net         *net.Server
	Listener    libNet.Listener
	tlsListener *Listener
	errors      chan error
	wg          sync.WaitGroup

	StopTimeout time.Duration
	KillTimeout time.Duration
	Timer       clock.Clock

	origConnState func(libNet.Conn, libHttp.ConnState)

	new    chan libNet.Conn
	active chan libNet.Conn
	idle   chan libNet.Conn
	closed chan libNet.Conn
	stop   chan chan struct{}
	kill   chan chan struct{}

	serveErr chan error

	stopOnce sync.Once
	stopErr  error
}

func NewServer(addr string, timeout time.Duration, stopTimeout time.Duration, killTimeout time.Duration, handler libHttp.Handler, timer clock.Clock) (srv *Server) {
	if stopTimeout == 0 {
		stopTimeout = DefaultStopTimeout
	}
	if killTimeout == 0 {
		killTimeout = DefaultKillTimeout
	}
	if timer == nil {
		timer = clock.New()
	}
	srv = &Server{
		StopTimeout: stopTimeout,
		KillTimeout: killTimeout,
		Timer:       timer,

		errors: make(chan error, 1+2),

		new:    make(chan libNet.Conn),
		active: make(chan libNet.Conn),
		idle:   make(chan libNet.Conn),
		closed: make(chan libNet.Conn),
		stop:   make(chan chan struct{}),
		kill:   make(chan chan struct{}),

		serveErr: make(chan error, 1),
	}

	srv.Net = &net.Server{}
	// Http Server
	srv.Server = &libHttp.Server{}
	// overwrite ConnState
	srv.origConnState = srv.Server.ConnState
	srv.Server.ConnState = srv.connState
	// Addr
	srv.Server.Addr = addr
	// Handler
	srv.Server.Handler = handler
	// Timeout
	srv.Server.ReadTimeout = timeout
	srv.Server.WriteTimeout = timeout
	//srv.Server.SetKeepAlivesEnabled(true)

	return srv
}

// Serve will serve the given http.Servers and will monitor for signals
// allowing for graceful termination (SIGTERM) or restart (SIGUSR2).
func (s *Server) ListenAndServe() (err error) {
	go s.handleSignals()

	if s.Server.Addr == "" {
		s.Server.Addr = ":http"
	}

	// Acquire Listeners
	s.Listener, err = newListener(s.Server.Addr)
	if err != nil {
		log.Println(err)
		return err
	}

	if didInherit {
		if ppid == 1 {
			log.Printf("Listening on init activated %s", printAddr(s.Listener))
		} else {
			const msg = "Graceful handoff of %s with new pid %d and old pid %d"
			log.Printf(msg, printAddr(s.Listener), os.Getpid(), ppid)
		}
	} else {
		const msg = "Serving %s with pid %d"
		log.Printf(msg, printAddr(s.Listener), os.Getpid())
	}

	// Start serving.
	go s.Manage()
	s.Serve()

	return nil
}

// ListenAndServeTLS listens on the TCP network address srv.Addr and then calls
// Serve to handle requests on incoming TLS connectionsrv.
//
// Filenames containing a certificate and matching private key for the server must
// be provided. If the certificate is signed by a certificate authority, the
// certFile should be the concatenation of the server's certificate followed by the
// CA's certificate.
//
// If srv.Addr is blank, ":https" is used.
func (s *Server) ListenAndServeTLS(certFile, keyFile string) (err error) {
	go s.handleSignals()

	if s.Server.Addr == "" {
		s.Server.Addr = ":https"
	}

	if s.Server.TLSConfig == nil {
		s.Server.TLSConfig = &tls.Config{}
	}
	if s.Server.TLSConfig.NextProtos == nil {
		s.Server.TLSConfig.NextProtos = []string{"http/1.1"}
	}

	s.Server.TLSConfig.Certificates = make([]tls.Certificate, 1)
	s.Server.TLSConfig.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	s.tlsListener, err = newListener(s.Server.Addr)
	if err != nil {
		log.Println(err)
		return err
	}

	s.Listener = tls.NewListener(s.tlsListener, s.Server.TLSConfig)

	if didInherit {
		if ppid == 1 {
			log.Printf("Listening on init activated %s", printAddr(s.Listener))
		} else {
			const msg = "Graceful handoff of %s with new pid %d and old pid %d"
			log.Printf(msg, printAddr(s.Listener), os.Getpid(), ppid)
		}
	} else {
		const msg = "Serving %s with pid %d"
		log.Printf(msg, printAddr(s.Listener), os.Getpid())
	}

	go s.Manage()
	s.Serve()

	return nil
}
func (s *Server) connState(c libNet.Conn, cs libHttp.ConnState) {
	if s.origConnState != nil {
		s.origConnState(c, cs)
	}

	switch cs {
	case libHttp.StateNew:
		s.new <- c
	case libHttp.StateActive:
		s.active <- c
	case libHttp.StateIdle:
		s.idle <- c
	case libHttp.StateHijacked, libHttp.StateClosed:
		s.closed <- c
	}
}

func (s *Server) Manage() {
	defer func() {
		close(s.new)
		close(s.active)
		close(s.idle)
		close(s.closed)
		close(s.stop)
		close(s.kill)
	}()

	var stopDone chan struct{}

	conns := map[libNet.Conn]libHttp.ConnState{}
	var countNew, countActive, countIdle float64

	// decConn decrements the count associated with the current state of the
	// given connection.
	decConn := func(c libNet.Conn) {
		switch conns[c] {
		default:
			panic(fmt.Errorf("unknown existing connection: %s", c))
		case libHttp.StateNew:
			countNew--
		case libHttp.StateActive:
			countActive--
		case libHttp.StateIdle:
			countIdle--
		}
	}

	incConn := func(c libNet.Conn) {
		switch conns[c] {
		default:
			panic(fmt.Errorf("unknown existing connection: %s", c))
		case libHttp.StateNew:
			countNew++
		case libHttp.StateActive:
			countActive++
		case libHttp.StateIdle:
			countIdle++
		}
	}

	log.Printf("Manage State.")
	for {
		select {
		case c := <-s.new:
			//log.Printf("connection established")
			conns[c] = libHttp.StateNew
			incConn(c)
			s.wg.Add(1)
		case c := <-s.active:
			//log.Printf("connection processing")
			decConn(c)
			conns[c] = libHttp.StateActive
			incConn(c)
		case c := <-s.idle:
			//log.Printf("connection idle")
			decConn(c)
			conns[c] = libHttp.StateIdle
			incConn(c)
			// if we're already stopping, close it
			if stopDone != nil {
				c.Close()
			}
		case c := <-s.closed:
			//log.Printf("connection closed")
			decConn(c)
			delete(conns, c)
			incConn(c)
			s.wg.Done()

			// if we're waiting to stop and are all empty, we just closed the last
			// connection and we're done.
			if stopDone != nil && len(conns) == 0 {
				close(stopDone)
				return
			}
		case stopDone = <-s.stop:
			// if we're already all empty, we're already done
			if len(conns) == 0 {
				close(stopDone)
				return
			}

			// close current idle connections right away
			for c, cs := range conns {
				if cs == libHttp.StateIdle {
					c.Close()
				}
			}

			// continue the loop and wait for all the ConnState updates which will
			// eventually close(stopDone) and return from this goroutine.

		case killDone := <-s.kill:
			// force close all connections
			for c, _ := range conns {
				c.Close()
			}

			// don't block the kill.
			close(killDone)

			// continue the loop and we wait for all the ConnState updates and will
			// return from this goroutine when we're all done. otherwise we'll try to
			// send those ConnState updates on closed channels.

		}
	}
}

func (s *Server) Serve() (err error) {
	s.serveErr <- s.Server.Serve(s.Listener)
	close(s.serveErr)

	// Close the parent if we inherited and it wasn't init that started us.
	if didInherit && ppid != 1 {
		if err := syscall.Kill(ppid, syscall.SIGTERM); err != nil {
			return fmt.Errorf("failed to close parent: %s", err)
		}
	}
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		s.Wait()
	}()
	select {
	case err := <-s.errors:
		if err == nil {
			panic("unexpected nil error")
		}
		return err
	case <-waitDone:
		log.Printf("Exiting connections on pid %d...", syscall.Getpid())
		return nil
	}
	return nil
}

func (s *Server) Wait() {
	go func(s *Server) {
		if err := <-s.serveErr; !isUseOfClosedError(err) {
			s.errors <- err
		}
	}(s)
	log.Printf("Waiting for connections on pid %d to finish...", syscall.Getpid())
	s.wg.Wait()
}

func (s *Server) Term() {
	go func(s *Server) {
		if err := s.Stop(); err != nil {
			s.errors <- err
		}
	}(s)
}

func (s *Server) Stop() error {
	s.stopOnce.Do(func() {
		// first disable keep-alive for new connections
		s.Server.SetKeepAlivesEnabled(false)

		// then close the listener so new connections can't connect come thru
		closeErr := s.Listener.Close()

		// then trigger the background goroutine to stop and wait for it
		stopDone := make(chan struct{})
		s.stop <- stopDone
		// wait for stop
		select {
		case <-stopDone:
			log.Printf("Stoped service on pid %d ", syscall.Getpid())
		case <-s.Timer.After(s.StopTimeout):
			log.Printf("Stop service on pid %d timeout after %d s", syscall.Getpid(), s.StopTimeout)
			// stop timed out, wait for kill
			killDone := make(chan struct{})
			s.kill <- killDone
			select {
			case <-killDone:
				log.Printf("Killed service on pid %d ", syscall.Getpid())
			case <-s.Timer.After(s.KillTimeout):
				log.Printf("Kill service on pid %d timeout after %d s", syscall.Getpid(), s.StopTimeout)
				// kill timed out, give up
			}
		}

		if closeErr != nil && !isUseOfClosedError(closeErr) {
			s.stopErr = closeErr
		}
	})
	return s.stopErr
}

func (s *Server) handleSignals() {
	sigCh := make(chan os.Signal, 10)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR2)
	log.Println("Handle Signals.")
	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGINT:
			// this ensures a subsequent INT will trigger standard go behaviour of
			// terminating.
			log.Printf("Received SIGINT on pid %d.", syscall.Getpid())
			if err := s.Stop(); err != nil {
				log.Println(err.Error())
			}
			s.Term()
			return
		case syscall.SIGTERM:
			// this ensures a subsequent TERM will trigger standard go behaviour of
			// terminating.
			log.Printf("Received SIGTERM on pid %d.", syscall.Getpid())
			if err := s.Stop(); err != nil {
				log.Println(err.Error())
			}
			s.Term()
			return
		case syscall.SIGUSR2:
			log.Printf("Received SIGUSR2 on pid %d.", syscall.Getpid())
			// we only return here if there's an error, otherwise the new process
			// will send us a TERM when it's ready to trigger the actual shutdown.
			if _, err := s.Net.StartProcess(); err != nil {
				s.errors <- err
			}
		}
	}
}

// Used for pretty printing addresses.
func printAddr(l libNet.Listener) []byte {
	var out bytes.Buffer
	fmt.Fprint(&out, l.Addr())
	return out.Bytes()
}
func isUseOfClosedError(err error) bool {
	if err == nil {
		return false
	}
	if opErr, ok := err.(*libNet.OpError); ok {
		err = opErr.Err
	}
	return err.Error() == "use of closed network connection"
}

// ListenAndServe refer http.ListenAndServe
func ListenAndServe(addr string, timeout time.Duration, handler libHttp.Handler) error {
	server := NewServer(addr, timeout, 2*timeout, 4*timeout, handler, nil)
	return server.ListenAndServe()
}

// ListenAndServeTLS refer http.ListenAndServeTLS
func ListenAndServeTLS(addr string, timeout time.Duration, certFile string, keyFile string, handler libHttp.Handler) error {
	server := NewServer(addr, timeout, 2*timeout, 4*timeout, handler, nil)
	return server.ListenAndServeTLS(certFile, keyFile)
}
