package transport

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// testTarget is a Target good enough for tests that never actually
// dial anything real -- its host/path only ever show up in error
// messages and sshArgs.
var testTarget = Target{Host: "example.com", Path: "/home/alice/project"}

// TestSSHArgs checks the argument list SSHDialer hands to `ssh`: -T (no
// pty), any --ssh-opt values, user@host[:port], then the remote command.
func TestSSHArgs(t *testing.T) {
	t.Parallel()

	target := Target{User: "alice", Host: "example.com", Port: "2222", Path: "/srv/project"}
	opts := DialerOptions{RemoteBin: "sennit-custom", SSHOpts: []string{"StrictHostKeyChecking=no"}}

	got := sshArgs(target, opts)
	want := []string{
		"-T", "-p", "2222",
		"-o", "StrictHostKeyChecking=no",
		"alice@example.com", "--",
		"sennit-custom", "daemon", "bridge", "--cwd", "/srv/project",
	}
	if len(got) != len(want) {
		t.Fatalf("sshArgs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sshArgs[%d] = %q, want %q (full: %q)", i, got[i], want[i], got)
		}
	}
}

// TestSSHDialer_EchoRoundTrip proves the net.Conn adapter works over a
// real subprocess's stdio: `cat` stands in for a well-behaved `ssh ...
// daemon bridge`, copying whatever is written to its stdin back out its
// stdout.
func TestSSHDialer_EchoRoundTrip(t *testing.T) {
	t.Parallel()

	dialer := SSHDialer(testTarget, DialerOptions{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "cat")
		},
	})

	conn, err := dialer(context.Background(), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello over the bridge")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, len(msg))
	if err := readFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf, msg) {
		t.Fatalf("echoed %q, want %q", buf, msg)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) error {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return err
		}
	}
	return nil
}

// TestSSHDialer_DialErrorSurfacesStderr checks that once the ssh process
// exits, a blocked Read/Write returns an error quoting its stderr --
// exactly the case a real `ssh` prints "Permission denied" and exits
// 255 for.
func TestSSHDialer_DialErrorSurfacesStderr(t *testing.T) {
	t.Parallel()

	dialer := SSHDialer(testTarget, DialerOptions{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "echo 'Permission denied' >&2; exit 255")
		},
	})

	conn, err := dialer(context.Background(), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 16)
	err = readUntilErr(conn, buf, 2*time.Second)
	if err == nil {
		t.Fatal("expected an error once the ssh process exits, got nil")
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("error %q does not quote the process's stderr", err)
	}
}

func readUntilErr(conn interface{ Read([]byte) (int, error) }, buf []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := conn.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return errors.New("timed out waiting for an error")
}

// TestSSHDialer_ReadDeadlineClosesConn checks the fallback documented on
// sshConn.SetReadDeadline: a deadline in the past closes the connection
// rather than leaving a blocked Read hanging forever against a wedged
// process.
func TestSSHDialer_ReadDeadlineClosesConn(t *testing.T) {
	t.Parallel()

	dialer := SSHDialer(testTarget, DialerOptions{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			// sleep stands in for a wedged ssh: it neither writes nor
			// exits within the test's own patience.
			return exec.CommandContext(ctx, "sleep", "30")
		},
	})

	conn, err := dialer(context.Background(), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, 1)
	err = readUntilErr(conn, buf, 2*time.Second)
	if err == nil {
		t.Fatal("expected a past read deadline to close the conn")
	}
}

// TestSSHDialer_SetDeadlineThenClear_ConnStaysUsable checks the property
// a review of grpc-go's own client transport asked for directly: gRPC's
// server-side accept path (grpc-go's server.go, rawConn.SetDeadline
// then SetDeadline(time.Time{}) once the handshake completes) is the
// general shape of "arm a deadline, then clear it before it fires, and
// the conn must stay healthy" -- so armDeadline must cancel a live timer
// on ANY later SetDeadline call, not just stop it when the new time is
// itself in the past. (On the client transport this package actually
// feeds, http2Client.Close is the only caller of conn.SetDeadline, and it
// never clears afterward -- it's tearing the conn down anyway, so a fired
// timer there is harmless. This test pins the general primitive rather
// than that one caller's happenstance, since nothing stops a future gRPC
// version from clearing a deadline on a conn it intends to keep.)
func TestSSHDialer_SetDeadlineThenClear_ConnStaysUsable(t *testing.T) {
	t.Parallel()

	dialer := SSHDialer(testTarget, DialerOptions{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "cat")
		},
	})
	conn, err := dialer(context.Background(), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Arm a deadline that would fire soon, then clear it before it does.
	if err := conn.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("SetDeadline(zero): %v", err)
	}

	// Wait past the original deadline. If the timer wasn't actually
	// canceled, Close would have fired by now and every Read/Write below
	// fails.
	time.Sleep(400 * time.Millisecond)

	msg := []byte("still usable after the cleared deadline")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write after clearing the deadline: %v", err)
	}
	buf := make([]byte, len(msg))
	if err := readFull(conn, buf); err != nil {
		t.Fatalf("read after clearing the deadline: %v", err)
	}
	if !bytes.Equal(buf, msg) {
		t.Fatalf("echoed %q, want %q", buf, msg)
	}
}

// TestSSHDialer_SetDeadlineLaterReplacesEarlier checks the other half of
// armDeadline's re-arm behavior: setting a LATER deadline while an
// earlier one is still pending must cancel the earlier timer, not leave
// both running (which would otherwise close the conn at the first,
// stale deadline despite the caller explicitly pushing it back).
func TestSSHDialer_SetDeadlineLaterReplacesEarlier(t *testing.T) {
	t.Parallel()

	dialer := SSHDialer(testTarget, DialerOptions{
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "cat")
		},
	})
	conn, err := dialer(context.Background(), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline (later): %v", err)
	}

	// Past the first deadline, before the second: the conn must still be
	// usable.
	time.Sleep(300 * time.Millisecond)
	msg := []byte("ok")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write between the two deadlines: %v", err)
	}
	buf := make([]byte, len(msg))
	if err := readFull(conn, buf); err != nil {
		t.Fatalf("read between the two deadlines: %v", err)
	}
}
