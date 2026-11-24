package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
)

// In restricted CI, exercise both HTTP transports over a full-duplex pipe.
// The default suite still binds real loopback sockets.
var pipeEndpoints sync.Map
var pipePort atomic.Int32

type pipeListener struct {
	addr   net.TCPAddr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeListener) Addr() net.Addr { return &l.addr }
func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed); pipeEndpoints.Delete(l.addr.String()) })
	return nil
}
func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func enablePipeTransport() {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		value, ok := pipeEndpoints.Load(address)
		if !ok {
			return nil, net.ErrClosed
		}
		l := value.(*pipeListener)
		client, server := net.Pipe()
		select {
		case l.conns <- server:
			return client, nil
		case <-ctx.Done():
		case <-l.closed:
		}
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	}
	http.DefaultTransport = tr
}
func testListener(h http.Handler) *httptest.Server {
	if os.Getenv("GATEWAY_TEST_PIPE") != "1" {
		return httptest.NewUnstartedServer(h)
	}
	l := &pipeListener{addr: net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 20000 + int(pipePort.Add(1))}, conns: make(chan net.Conn), closed: make(chan struct{})}
	pipeEndpoints.Store(l.addr.String(), l)
	return &httptest.Server{Listener: l, Config: &http.Server{Handler: h}}
}
func testHTTP(h http.Handler) *httptest.Server { s := testListener(h); s.Start(); return s }
