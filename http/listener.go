package http

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

type Listener struct {
	net.Listener

	stop    chan error
	stopped bool
}

func newListener(addr string) (l *Listener, err error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		err = fmt.Errorf("net.Listen error: %v", err)
		return nil, err
	}
	l = &Listener{
		Listener: listener,

		stop: make(chan error),
	}
	go func() {
		_ = <-l.stop
		l.stopped = true
		l.stop <- l.Listener.Close()
	}()
	return l, nil
}

func (l *Listener) Accept() (c net.Conn, err error) {
	tcpConn, err := l.Listener.(*net.TCPListener).AcceptTCP()
	if err != nil {
		return nil, err
	}

	tcpConn.SetKeepAlive(true)
	tcpConn.SetKeepAlivePeriod(3 * time.Minute)

	return tcpConn, nil
}

func (l *Listener) Close() error {
	if l.stopped {
		return syscall.EINVAL
	}
	l.stop <- nil
	return <-l.stop
}

func (l *Listener) File() *os.File {
	// returns a dup(2) - FD_CLOEXEC flag *not* set
	tl := l.Listener.(*net.TCPListener)
	fl, _ := tl.File()
	return fl
}
