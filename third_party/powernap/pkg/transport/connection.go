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
// to notice the forcibly closed stream and tear itself down. It only guards
// against an unexpected read-loop hang after the process has been reaped.
const disconnectWait = 10 * time.Second

// Connection represents a managed connection to a language server.
type Connection struct {
	conn      jsonrpc2.JSONRPC2
	transport *Transport
	router    *Router
	logger    *slog.Logger

	// stream is the raw process stream jsonrpc2 reads from. Forced Close
	// interrupts it directly rather than going through conn - see Close.
	stream  io.Closer
	process interface {
		ForceClose() error
		Wait(context.Context) error
	}
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
func NewConnection(_ context.Context, stream io.ReadWriteCloser, logger *slog.Logger) (*Connection, error) {
	c := &Connection{
		router:   NewRouter(),
		logger:   logger,
		requests: make(map[jsonrpc2.ID]chan *Message),
		stream:   stream,
	}
	c.process, _ = stream.(interface {
		ForceClose() error
		Wait(context.Context) error
	})

	// Suppress or redirect jsonrpc2 log messages to our logger.
	// Otherwise, jsonrpc2 might print to stderr and mess with the application
	// view if any.
	stdLogger := log.New(io.Discard, "", 0)
	if logger != nil {
		stdLogger = slog.NewLogLogger(logger.Handler(), slog.LevelDebug)
	}

	// Create JSON-RPC connection
	// Do not give jsonrpc2 a cancellation context. Its context watcher calls
	// Conn.close directly, which can race readMessages delivering a response and
	// close the same pending-call channel. Connection.Close owns teardown by
	// closing the process stream and waiting for the read loop to self-close.
	conn := jsonrpc2.NewConn(
		context.Background(),
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
// Sennit-local change: sourcegraph/jsonrpc2 v0.2.2's Conn.close() clears
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
// calls Conn.close() itself, never racing its own delivery. If the read loop
// does not notice within disconnectWait, return an error rather than invoking
// the unsafe external Conn.Close fallback.
func (c *Connection) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	if c.closed.Load() {
		return c.closeErr
	}

	c.closed.Store(true)

	var streamErr error
	if c.process != nil {
		streamErr = c.process.ForceClose()
	} else if c.stream != nil {
		streamErr = c.stream.Close()
	}

	if c.disconnect != nil {
		select {
		case <-c.disconnect:
			// The read loop saw the stream close and tore itself down.
		case <-time.After(disconnectWait):
			c.closeErr = fmt.Errorf("jsonrpc2 read loop did not stop after forced stream close")
		}
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

// WaitForDisconnect waits until jsonrpc2's read loop has observed peer EOF.
func (c *Connection) WaitForDisconnect(ctx context.Context) error {
	if c.disconnect == nil {
		return fmt.Errorf("connection has no disconnect notification")
	}
	select {
	case <-c.disconnect:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForProcess finishes the peer-initiated stream close, then waits for and
// reaps the process without terminating it. DisconnectNotify is closed before
// jsonrpc2 closes its ObjectStream, so the explicit Close also synchronizes
// with that in-progress cleanup before cmd.Wait starts.
func (c *Connection) WaitForProcess(ctx context.Context) error {
	if c.process == nil {
		return nil
	}
	if err := c.stream.Close(); err != nil {
		return err
	}
	return c.process.Wait(ctx)
}

// IsConnected returns true if the connection is still active.
func (c *Connection) IsConnected() bool {
	return !c.closed.Load()
}
