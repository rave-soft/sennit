package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

// defaultRemoteBin is the remote binary SSHDialer invokes `daemon
// bridge` on when Options.RemoteBin is unset.
const defaultRemoteBin = "sennit"

// maxStderrCapture bounds how much of a failed ssh process's stderr
// DialerOptions keeps around for a dial error to quote -- enough for a
// host-key prompt or "Permission denied" message, not an unbounded sink
// for a chatty remote shell (a login MOTD, say).
const maxStderrCapture = 4096

// DialerOptions configures SSHDialer.
type DialerOptions struct {
	// RemoteBin is the binary name invoked on the remote host as
	// `<RemoteBin> daemon bridge --cwd <path>`. Defaults to "sennit".
	RemoteBin string

	// SSHOpts are extra `-o key=value` options appended to the ssh
	// invocation, in order, before the destination -- for a caller who
	// wants StrictHostKeyChecking, IdentitiesOnly, etc. set per attach
	// rather than in ~/.ssh/config.
	SSHOpts []string

	// Command, if set, replaces exec.CommandContext as how the ssh
	// process itself is started -- a test hook, mirroring
	// supervisor.Options.Command, so a test can substitute a fake `ssh`
	// (a re-exec'd test binary) for the real one. Called with "ssh" and
	// the full argument list SSHDialer would otherwise pass to
	// exec.CommandContext, and always context.Background() (see
	// dialSSH's doc comment on why the process must outlive gRPC's own
	// dial context).
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func (o DialerOptions) remoteBin() string {
	if o.RemoteBin != "" {
		return o.RemoteBin
	}
	return defaultRemoteBin
}

func (o DialerOptions) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if o.Command != nil {
		return o.Command(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...) //nolint:gosec // name is always "ssh"; args are built here, not from unvalidated user input.
}

// sshArgs builds the argument list SSHDialer passes to `ssh`: -T (no
// pty -- this is a byte pipe, not an interactive shell) and BatchMode=no
// (see this func's own note below), then any Options.SSHOpts, then the
// destination and the remote command.
//
// BatchMode is left at its default (no) rather than forced on: a
// password or 2FA prompt from the remote host goes to ssh's controlling
// terminal, not to the pipes this package reads/writes -- exactly the
// same terminal the person running `sennit attach ssh://...` is already
// sitting at, since this process was launched from it. Forcing
// BatchMode=yes would make such a prompt fail outright instead of asking
// the person for it, so it is left for their own ssh config (or
// --ssh-opt BatchMode=yes) to decide, same as a plain `ssh host` would.
func sshArgs(target Target, opts DialerOptions) []string {
	args := []string{"-T"}
	if target.Port != "" {
		args = append(args, "-p", target.Port)
	}
	for _, o := range opts.SSHOpts {
		args = append(args, "-o", o)
	}
	host := target.Host
	if target.User != "" {
		host = target.User + "@" + host
	}
	args = append(args, host, "--", opts.remoteBin(), "daemon", "bridge", "--cwd", target.Path)
	return args
}

// SSHDialer returns a grpc.WithContextDialer-compatible func that starts
// a fresh `ssh ... -- <remoteBin> daemon bridge --cwd <path>` process on
// every call and returns a net.Conn adapter over its stdin/stdout
// (CLIENT-SERVER.md, PR 3.1). Each dial starts its own process rather
// than reusing one, so gRPC's own reconnect logic -- which redials on a
// dropped connection -- transparently gets a fresh SSH session each
// time, with no bridging code of its own needing to notice a reconnect
// happened.
//
// It uses the system `ssh` binary (not a Go SSH client), so the
// person's own ~/.ssh/config, keys, ProxyJump and agent all apply
// exactly as they would for `ssh host` typed by hand.
func SSHDialer(target Target, opts DialerOptions) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return dialSSH(ctx, target, opts)
	}
}

func dialSSH(ctx context.Context, target Target, opts DialerOptions) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// context.Background(), not ctx: gRPC's own dialer context is scoped
	// to establishing the transport, not to the connection's whole
	// lifetime (its cancellation timing is an implementation detail of
	// grpc-go, not part of the WithContextDialer contract) -- tying the
	// ssh process to it would kill a perfectly healthy connection out
	// from under an in-flight RPC the moment that context happens to be
	// canceled. The process's lifetime is instead owned by sshConn.Close,
	// which gRPC calls when it actually wants this connection gone
	// (mirrors supervisor.spawnDetached's own exec.CommandContext(
	// context.Background(), ...) note for the same reason).
	cmd := opts.command(context.Background(), "ssh", sshArgs(target, opts)...) // ok: detached - the ssh process outlives gRPC's own per-dial context; sshConn.Close (driven by the conn's users, not this call) owns its lifetime

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("transport: ssh stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("transport: ssh stdout pipe: %w", err)
	}
	stderr := &boundedBuffer{limit: maxStderrCapture}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("transport: start ssh to %s: %w", target, err)
	}

	c := &sshConn{
		target: target,
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		done:   make(chan struct{}),
	}
	go c.wait()
	return c, nil
}

// boundedBuffer is an io.Writer that keeps only the first limit bytes
// written to it -- used to capture a failed ssh process's stderr without
// letting a chatty remote unboundedly grow memory.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) > room {
			b.buf = append(b.buf, p[:room]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// sshConn adapts an `ssh` subprocess's stdin/stdout to a net.Conn, for
// gRPC's http2 transport to speak the wire protocol over. Reads and
// writes are the pipes themselves; Close tears the process down and
// waits for it, so a caller never leaks the child. wait watches for the
// process to exit on its own (the remote end closing the connection, an
// auth failure, a network drop) and marks the conn closed either way,
// so a blocked Read/Write unblocks instead of hanging on a dead process
// forever.
type sshConn struct {
	target Target
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *boundedBuffer

	closeOnce sync.Once
	done      chan struct{} // closed once cmd.Wait returns
	waitErr   error

	deadlineMu sync.Mutex
	rTimer     *time.Timer
	wTimer     *time.Timer
}

func (c *sshConn) wait() {
	c.waitErr = c.cmd.Wait()
	close(c.done)
}

// dialError reports why the ssh process ended, quoting its stderr --
// used by Read/Write once the process has exited, so a caller sees
// "Permission denied" (or whatever the remote said) instead of a bare
// EOF or "broken pipe".
func (c *sshConn) dialError() error {
	// stdout hitting EOF and cmd.Wait() actually returning are two
	// separate events (the child closes its stdout fd, then the kernel
	// reaps it) -- a short, bounded wait here closes that gap instead of
	// racing it: an EOF from a process that is mid-exit almost always
	// means Wait() is about to return too, and this is the only chance
	// to attach its stderr/exit status to the error the caller sees.
	select {
	case <-c.done:
	case <-time.After(500 * time.Millisecond):
		return nil
	}
	msg := c.stderr.String()
	if c.waitErr == nil && msg == "" {
		return nil
	}
	if msg != "" {
		return fmt.Errorf("transport: ssh to %s exited: %w: %s", c.target, c.waitErr, msg)
	}
	return fmt.Errorf("transport: ssh to %s exited: %w", c.target, c.waitErr)
}

func (c *sshConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	if err != nil {
		if dialErr := c.dialError(); dialErr != nil {
			return n, dialErr
		}
	}
	return n, err
}

func (c *sshConn) Write(p []byte) (int, error) {
	n, err := c.stdin.Write(p)
	if err != nil {
		if dialErr := c.dialError(); dialErr != nil {
			return n, dialErr
		}
	}
	return n, err
}

// Close ends the ssh process (closing its stdin first asks it to exit
// cleanly; Process.Kill follows if it hasn't within a bounded grace
// period, e.g. a wedged ssh that ignores EOF on stdin) and waits for it,
// so Close never returns while the child is still around.
func (c *sshConn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		c.deadlineMu.Lock()
		if c.rTimer != nil {
			c.rTimer.Stop()
		}
		if c.wTimer != nil {
			c.wTimer.Stop()
		}
		c.deadlineMu.Unlock()

		closeErr = c.stdin.Close()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-c.done
		}
	})
	return closeErr
}

func (c *sshConn) LocalAddr() net.Addr  { return sshAddr{"local", c.target} }
func (c *sshConn) RemoteAddr() net.Addr { return sshAddr{"remote", c.target} }

// SetDeadline/SetReadDeadline/SetWriteDeadline: gRPC's http2 transport
// calls these on every net.Conn it's handed (framer reads/writes go
// through them), but never relies on them to enforce an RPC's own
// timeout -- that's done above the transport, via context. What it does
// need is that a deadline set past a hung read/write eventually
// unblocks it, the way a real socket's deadline would; a plain no-op
// here would leave a wedged ssh process (network partition, remote
// host frozen) blocking Read/Write forever with no way out. So a
// deadline is implemented as a timer that closes the connection when it
// fires -- coarser than a real per-call deadline (it ends the whole
// conn, not just the one blocked call), but that is exactly what gRPC
// itself does with a conn it decides is unhealthy, and closing forces
// exactly the reconnect (SSHDialer starts a fresh process) this
// package's own doc comment already describes as the recovery path.
func (c *sshConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *sshConn) SetReadDeadline(t time.Time) error {
	c.armDeadline(&c.rTimer, t)
	return nil
}

func (c *sshConn) SetWriteDeadline(t time.Time) error {
	c.armDeadline(&c.wTimer, t)
	return nil
}

func (c *sshConn) armDeadline(timer **time.Timer, t time.Time) {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if *timer != nil {
		(*timer).Stop()
		*timer = nil
	}
	if t.IsZero() {
		return
	}
	d := time.Until(t)
	if d <= 0 {
		go c.Close()
		return
	}
	*timer = time.AfterFunc(d, func() { _ = c.Close() })
}

// sshAddr is the net.Addr SetDeadline's doc comment above notwithstanding
// LocalAddr/RemoteAddr report -- neither end is a real socket address,
// so this names the SSH target instead, which is the only address that
// means anything for this conn.
type sshAddr struct {
	side   string
	target Target
}

func (a sshAddr) Network() string { return "ssh-" + a.side }
func (a sshAddr) String() string  { return a.target.String() }
