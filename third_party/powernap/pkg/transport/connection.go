// Package transport provides JSON-RPC 2.0 transport for LSP communication.
package transport

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

// disconnectWait bounds how long Close waits for jsonrpc2's own read loop
// to notice the closed stream and tear itself down (see the comment on
// Close). It only guards against an unexpected hang; the stream close it
// waits on already has its own bound (processCloser's 5s kill timeout).
const disconnectWait = 10 * time.Second

// Connection represents a managed connection to a language server.
type Connection struct {
	conn      jsonrpc2.JSONRPC2
	transport *Transport
	router    *Router
	logger    *slog.Logger

	// stream is the raw process stream jsonrpc2 reads from. Close closes
	// it directly rather than going through conn - see Close.
	stream io.Closer
	// disconnect is closed by jsonrpc2 once its read loop has torn the
	// connection down, whether that happens on its own (a read error) or
	// via conn.Close(). Close waits on it instead of calling conn.Close()
	// itself.
	disconnect <-chan struct{}

	// State management
	closed   atomic.Bool
	closeMu  sync.Mutex
	closeErr error

	// Request tracking
	requestMu sync.Mutex
	requests  map[jsonrpc2.ID]chan *Message
}

// NewConnection creates a new managed connection.
func NewConnection(ctx context.Context, stream io.ReadWriteCloser, logger *slog.Logger) (*Connection, error) {
	c := &Connection{
		router:   NewRouter(),
		logger:   logger,
		requests: make(map[jsonrpc2.ID]chan *Message),
		stream:   stream,
	}

	// Suppress or redirect jsonrpc2 log messages to our logger.
	// Otherwise, jsonrpc2 might print to stderr and mess with the application
	// view if any.
	stdLogger := log.New(io.Discard, "", 0)
	if logger != nil {
		stdLogger = slog.NewLogLogger(logger.Handler(), slog.LevelDebug)
	}

	// Create JSON-RPC connection
	conn := jsonrpc2.NewConn(
		ctx,
		jsonrpc2.NewBufferedStream(stream, jsonrpc2.VSCodeObjectCodec{}),
		jsonrpc2.HandlerWithError(c.handleRequest),
		jsonrpc2.SetLogger(stdLogger),
	)

	c.conn = conn
	c.disconnect = conn.DisconnectNotify()
	c.transport = NewWithConn(conn)

	return c, nil
}

// Call makes a request to the language server and waits for a response.
func (c *Connection) Call(ctx context.Context, method string, params any, result any) error {
	if c.closed.Load() {
		return fmt.Errorf("connection is closed")
	}

	return c.conn.Call(ctx, method, params, result) //nolint:wrapcheck
}

// Notify sends a notification to the language server.
func (c *Connection) Notify(ctx context.Context, method string, params any) error {
	if c.closed.Load() {
		return fmt.Errorf("connection is closed")
	}

	return c.conn.Notify(ctx, method, params) //nolint:wrapcheck
}

// handleRequest handles incoming requests from the language server.
func (c *Connection) handleRequest(ctx context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (any, error) {
	if c.logger != nil {
		c.logger.Debug("Handling request", "method", req.Method)
	}

	return c.router.Route(ctx, req)
}

// RegisterHandler registers a handler for a specific method.
func (c *Connection) RegisterHandler(method string, handler Handler) {
	c.router.Handle(method, handler)
}

// RegisterNotificationHandler registers a notification handler.
func (c *Connection) RegisterNotificationHandler(method string, handler NotificationHandler) {
	c.router.HandleNotification(method, handler)
}

// Close closes the connection.
//
// Sennit-local change: sourcegraph/jsonrpc2 v0.2.1's Conn.close() clears
// every pending call's done channel without deleting it from c.pending,
// and its read loop delivers a response by deleting the pending entry and
// then writing to, and closing, that same done channel - all outside any
// lock. Calling conn.Close() from here, concurrently with the read loop
// mid-delivery, can make both touch the same channel and panic on a
// double close. That race only exists because close() would be invoked
// from a goroutine other than the read loop; when the read loop closes
// itself (its Read fails), there is no concurrent goroutine to race.
//
// So instead of closing the jsonrpc2 conn directly, close the raw process
// stream first: that fails the read loop's next Read, and the read loop
// calls Conn.close() itself, never racing its own delivery. Only if the
// read loop doesn't notice within disconnectWait does this fall back to
// closing the conn directly, which reopens the race in that (unexpected)
// case but is still better than leaking the connection.
func (c *Connection) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	if c.closed.Load() {
		return c.closeErr
	}

	c.closed.Store(true)

	var streamErr error
	if c.stream != nil {
		streamErr = c.stream.Close()
	}

	switch {
	case c.disconnect != nil:
		select {
		case <-c.disconnect:
			// The read loop saw the stream close and tore itself down.
		case <-time.After(disconnectWait):
			if c.conn != nil {
				c.closeErr = c.conn.Close()
			}
		}
	case c.conn != nil:
		c.closeErr = c.conn.Close()
	}

	if c.closeErr == nil {
		c.closeErr = streamErr
	}

	// Close any pending requests
	c.requestMu.Lock()
	for _, ch := range c.requests {
		close(ch)
	}
	c.requests = nil
	c.requestMu.Unlock()

	return c.closeErr
}

// IsConnected returns true if the connection is still active.
func (c *Connection) IsConnected() bool {
	return !c.closed.Load()
}
