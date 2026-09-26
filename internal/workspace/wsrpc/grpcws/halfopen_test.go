// halfOpenConn/halfOpenDialer simulate a half-open connection: the
// remote end vanished without sending a FIN or RST (the common case over
// a dropped SSH tunnel, or a machine that lost power), as opposed to
// severableDialer's sever() (events_stream_test.go), which calls the real
// Close() and so does send a clean half-close through bufconn's own pipe.
// CLIENT-SERVER.md, PR 1.3's "Уточнено ревью (п. 3)": only gRPC keepalive
// -- not an open RPC/stream, which a half-open connection still has --
// can ever notice this case.
package grpcws_test

import (
	"context"
	"errors"
	"net"
	"sync"

	"google.golang.org/grpc/test/bufconn"
)

// halfOpenConn wraps a bufconn client connection so that, once cut, every
// Write silently succeeds without forwarding a single byte: the server
// never receives so much as a PING ack, but nothing observable happens on
// this end either (no error, no Close) -- exactly what a client whose
// network has actually died looks like from here. Read is left
// untouched: whatever the server still manages to write into the shared
// pipe before it eventually gives up (its own keepalive PINGs, say) is
// harmless to let through, since this package's tests never depend on
// what a cut connection's owner sees afterward -- only on what the server
// concludes.
type halfOpenConn struct {
	net.Conn

	mu  sync.Mutex
	cut bool
}

func (c *halfOpenConn) cutNow() {
	c.mu.Lock()
	c.cut = true
	c.mu.Unlock()
}

func (c *halfOpenConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	cut := c.cut
	c.mu.Unlock()
	if cut {
		return len(b), nil
	}
	return c.Conn.Write(b)
}

// halfOpenDialer hands out halfOpenConns and, once cut, refuses every
// further dial -- standing in for the dialer of a client whose network
// path is simply gone, so gRPC's own reconnect logic can't silently heal
// the connection the way it would against a still-live bufconn.Listener
// (see TestSubscribe_ReconnectWithoutLoss, which relies on exactly that
// healing).
type halfOpenDialer struct {
	lis *bufconn.Listener

	mu      sync.Mutex
	conns   []*halfOpenConn
	refused bool
}

func (d *halfOpenDialer) dial(ctx context.Context, _ string) (net.Conn, error) {
	d.mu.Lock()
	refused := d.refused
	d.mu.Unlock()
	if refused {
		return nil, errors.New("halfOpenDialer: refusing to dial after cut, simulating no redial")
	}
	conn, err := d.lis.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &halfOpenConn{Conn: conn}
	d.mu.Lock()
	d.conns = append(d.conns, wrapped)
	d.mu.Unlock()
	return wrapped, nil
}

// cut flips every connection this dialer has handed out into swallow
// mode and stops it from dialing again.
func (d *halfOpenDialer) cut() {
	d.mu.Lock()
	d.refused = true
	conns := d.conns
	d.mu.Unlock()
	for _, c := range conns {
		c.cutNow()
	}
}
