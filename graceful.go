// Package gracehttp provides easy to use graceful restart
// functionality for HTTP server.
package graceful

import (
	"net"
	"net/http"
	"os"
	"time"

	libGraceful "github.com/tylerb/graceful"
)

type LogFunc func(format string, args ...interface{})
type ConnStateFunc func(net.Conn, http.ConnState)
type BeforeShutdownFunc func() bool
type ShutdownInitiatedFunc func()

type GracefulBuilder interface {
	Server(server *http.Server) GracefulBuilder
	Timeout(timeout time.Duration) GracefulBuilder
	ConnState(connStateFunc ConnStateFunc) GracefulBuilder
	BeforeShutdown(beforeShutdownFunc BeforeShutdownFunc) GracefulBuilder
	ShutdownInitiated(shutdownInitiatedFunc ShutdownInitiatedFunc) GracefulBuilder
	Interrupt(interrupt chan os.Signal) GracefulBuilder
	Build() *Graceful
}

type gracefulBuilder struct {
	server                *http.Server
	timeout               time.Duration
	logFunc               LogFunc
	connStateFunc         ConnStateFunc
	beforeShutdownFunc    BeforeShutdownFunc
	shutdownInitiatedFunc ShutdownInitiatedFunc
	interrupt             chan os.Signal
}

type Graceful struct {
	Server *libGraceful.Server
	Interrupt chan os.Signal
}

func New() GracefulBuilder {
	return &gracefulBuilder{}
}

func (g *gracefulBuilder) Server(server *http.Server) GracefulBuilder {
	g.server = server
	return g
}

func (g *gracefulBuilder) Timeout(timeout time.Duration) GracefulBuilder {
	g.timeout = timeout
	return g
}

func (g *gracefulBuilder) ConnState(connStateFunc ConnStateFunc) GracefulBuilder {
	g.connStateFunc = connStateFunc
	return g
}

func (g *gracefulBuilder) BeforeShutdown(beforeShutdownFunc BeforeShutdownFunc) GracefulBuilder {
	g.beforeShutdownFunc = beforeShutdownFunc
	return g
}

func (g *gracefulBuilder) ShutdownInitiated(shutdownInitiatedFunc ShutdownInitiatedFunc) GracefulBuilder {
	g.shutdownInitiatedFunc = shutdownInitiatedFunc
	return g
}

func (g *gracefulBuilder) Interrupt(interrupt chan os.Signal) GracefulBuilder {
	g.interrupt = interrupt
	return g
}

func (g *gracefulBuilder) Build() *Graceful {
	server := &libGraceful.Server{
		Server:            g.server,
		TCPKeepAlive:      3 * time.Minute,
		Timeout:           g.timeout,
		LogFunc:           g.logFunc,
		ConnState:         g.connStateFunc,
		BeforeShutdown:    g.beforeShutdownFunc,
		ShutdownInitiated: g.shutdownInitiatedFunc,
	}
	return &Graceful{
        Server: server,
		Interrupt: g.interrupt,
	}
}

func (g *Graceful) ListenAndServe() error {
	return g.Server.ListenAndServe()
}

func (g *Graceful) ListenAndServeTLS(certFile, keyFile string) error {
	return g.Server.ListenAndServeTLS(certFile, keyFile)
}

func (g *Graceful) Serve(listener net.Listener) error {
    return g.Server.Serve(listener)
}


