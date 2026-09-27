package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeUnixServer listens on a fresh unix socket, accepts exactly one
// connection, writes serverBytes to it, then reads whatever the client
// sends until EOF -- just enough of a "daemon socket" for
// runBridgeStreams to bridge without a real one.
//
// clientReceived is only safe to call after waitDone returns: the
// server's own io.Copy into its receive buffer runs on a separate
// goroutine that finishes independently of (and not necessarily before)
// runBridgeStreams returning on the client side -- reading the buffer
// without waiting first is a real, race-detector-visible data race, not
// just a timing nicety.
func fakeUnixServer(t *testing.T, serverBytes []byte) (socketPath string, clientReceived func() []byte, waitDone func()) {
	t.Helper()
	socketPath = filepath.Join(t.TempDir(), "fake.sock")
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })

	var received bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write(serverBytes)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		_, _ = io.Copy(&received, conn)
	}()

	waitDone = func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("fake unix server never finished")
		}
	}
	t.Cleanup(waitDone)
	return socketPath, func() []byte { return received.Bytes() }, waitDone
}

// TestRunBridgeStreams_StdoutIsExactlyTheSocketBytes is the "bridge
// stdout purity" check CLIENT-SERVER.md's PR 3.1 asks for: with an
// in-memory stdin and a fake socket server standing in for the daemon,
// what runBridgeStreams writes to stdout must be byte-for-byte what the
// server sent -- nothing prepended, appended, or interleaved, since
// stdout here is gRPC's own wire stream (this func's doc comment on
// daemonBridgeCmd).
func TestRunBridgeStreams_StdoutIsExactlyTheSocketBytes(t *testing.T) {
	t.Parallel()

	serverBytes := []byte("this is exactly what the daemon socket sent, frame and all")
	socketPath, clientReceived, waitDone := fakeUnixServer(t, serverBytes)

	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "unix", socketPath)
	require.NoError(t, err)

	clientBytes := []byte("this is what the ssh session's stdin carried")
	stdin := bytes.NewReader(clientBytes)
	var stdout bytes.Buffer

	err = runBridgeStreams(stdin, &stdout, conn)
	require.NoError(t, err)

	// The server side's own io.Copy into its receive buffer runs on a
	// different goroutine than runBridgeStreams -- wait for it explicitly
	// before reading clientReceived() (see fakeUnixServer's doc comment).
	waitDone()

	require.Equal(t, serverBytes, stdout.Bytes(), "stdout must be exactly the socket's bytes, nothing else")
	require.Equal(t, clientBytes, clientReceived(), "the socket must receive exactly stdin's bytes")
}
