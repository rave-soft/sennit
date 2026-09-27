package cmd

import (
	"context"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/testenv"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/transport"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// fakeSSHSocketEnv names the unix socket TestFakeSSHHelperProcess
// bridges stdio to -- set by fakeSSHCommand, read by the helper once
// re-exec'd as a subprocess pretending to be `ssh`.
const fakeSSHSocketEnv = "SENNIT_FAKE_SSH_SOCKET"

// fakeSSHCommand builds a transport.DialerOptions.Command hook that
// stands in for a real `ssh` invocation: it ignores every ssh argument
// SSHDialer would otherwise pass (host, -p, -o, the remote command) and
// instead re-execs this test binary as TestFakeSSHHelperProcess, which
// dials socketPath directly and bridges its stdio to it with the exact
// runBridgeStreams a real `sennit daemon bridge` uses server-side. This
// is what CLIENT-SERVER.md's PR 3.1 test plan calls "a fake ssh that
// ignores ssh args and execs the bridge function locally against a
// daemon started in t.TempDir()".
func fakeSSHCommand(t *testing.T, socketPath string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestFakeSSHHelperProcess") //nolint:gosec
		cmd.Env = append(os.Environ(), "SENNIT_FAKE_SSH_HELPER=1", fakeSSHSocketEnv+"="+socketPath)
		return cmd
	}
}

// TestFakeSSHHelperProcess is the subprocess fakeSSHCommand spawns
// (gated by SENNIT_FAKE_SSH_HELPER=1): it dials the unix socket named by
// fakeSSHSocketEnv and bridges its own stdin/stdout to it, exactly like
// `sennit daemon bridge` does once it has found the daemon's socket --
// this reuses runBridgeStreams itself, so the test exercises the real
// bridging code, not a reimplementation of it.
func TestFakeSSHHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_FAKE_SSH_HELPER") != "1" {
		return
	}
	socketPath := os.Getenv(fakeSSHSocketEnv)
	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "unix", socketPath)
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close()
	if err := runBridgeStreams(os.Stdin, os.Stdout, conn); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// TestConnectRemoteWorkspace_ThroughFakeSSH drives connectRemoteWorkspace
// through transport.SSHDialer with the real `ssh` binary replaced by
// fakeSSHCommand -- a real daemon (helperCommand's subprocess, the same
// one internal/cmd's other daemon tests use), a real unix socket, and a
// real bridging subprocess, just no real network hop. It checks the
// three things CLIENT-SERVER.md's PR 3.1 test plan asks of "remote
// attach setup through the fake ssh": Connect succeeds, a unary call
// works, and Subscribe delivers an event the daemon published (here, the
// session.Session from CreateSession) -- plus that ServerHome/WorkingDir
// read back as the daemon's own, not this test process's.
func TestConnectRemoteWorkspace_ThroughFakeSSH(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	// Start the real daemon subprocess directly through supervisor, the
	// same way daemonBridgeCmd's own EnsureRunning call would -- the
	// fake ssh below skips that step (it already knows the socket) so
	// this test's fixture controls exactly when the daemon exists.
	socketPath, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndRequestShutdown(t, context.Background(), socketPath) })

	target := transport.Target{Host: "fake-remote-host", Path: projectDir}
	dialerOpts := transport.DialerOptions{Command: fakeSSHCommand(t, socketPath)}

	client, prefs, cleanup, err := connectRemoteWorkspace(ctx, target, dialerOpts, "", false)
	require.NoError(t, err, "connectRemoteWorkspace over the fake ssh bridge")
	t.Cleanup(cleanup)
	require.NotNil(t, prefs)

	require.Equal(t, projectDir, client.WorkingDir(), "WorkingDir must read back as the remote daemon's project, not this test process's")

	hello, err := client.Hello(ctx)
	require.NoError(t, err)
	require.Equal(t, grpcws.ProtocolVersion, hello.ProtocolVersion)
	wantHome, homeErr := os.UserHomeDir()
	if homeErr == nil {
		require.Equal(t, wantHome, hello.ServerHome)
	} else {
		require.NotEmpty(t, hello.ServerHome)
	}

	events := make(chan any, 8)
	stop := client.SubscribeWith(func(msg any) { events <- msg })
	defer stop()

	created, err := client.CreateSession(ctx, "remote-through-fake-ssh")
	require.NoError(t, err, "unary call over the fake-ssh-bridged connection should succeed")

	deadline := time.After(daemonTestTimeout)
	for {
		select {
		case msg := <-events:
			if ev, ok := msg.(pubsub.Event[session.Session]); ok && ev.Payload.ID == created.ID {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the daemon's CreateSession event over Subscribe")
		}
	}
}

// sshSpawnTracker builds fake-ssh Command hooks like fakeSSHCommand's,
// but remembers every *exec.Cmd it hands back -- so a test can count how
// many ssh processes SSHDialer actually started (proving a reconnect
// spawned a fresh one, CLIENT-SERVER.md's own "each dial starts a new
// ssh process" contract) and kill the most recent one to force a
// reconnect.
type sshSpawnTracker struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
}

func (s *sshSpawnTracker) command(socketPath string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestFakeSSHHelperProcess") //nolint:gosec
		cmd.Env = append(os.Environ(), "SENNIT_FAKE_SSH_HELPER=1", fakeSSHSocketEnv+"="+socketPath)
		s.mu.Lock()
		s.cmds = append(s.cmds, cmd)
		s.mu.Unlock()
		return cmd
	}
}

func (s *sshSpawnTracker) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cmds)
}

// killLatest kills the most recently built ssh process. dialSSH calls
// Start on the *exec.Cmd this tracker already recorded (the same pointer,
// see command's own doc comment), so cmd.Process only becomes non-nil
// once Start has actually run -- killLatest polls for that, bounded by
// timeout, rather than assuming it is already there.
func (s *sshSpawnTracker) killLatest(t *testing.T, timeout time.Duration) {
	t.Helper()
	s.mu.Lock()
	cmd := s.cmds[len(s.cmds)-1]
	s.mu.Unlock()

	deadline := time.Now().Add(timeout)
	for cmd.Process == nil {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the fake ssh process to start, to kill it")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
}

// requireSessionEvent drains events until it sees a
// pubsub.Event[session.Session] for sessionID, discarding anything else
// (including the ConnectionEvent every subscribe/reconnect also
// produces) -- a test that only cares whether/when a particular session
// showed up, not the exact interleaving of everything else on the wire.
func requireSessionEvent(t *testing.T, events <-chan any, sessionID string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-events:
			if ev, ok := msg.(pubsub.Event[session.Session]); ok && ev.Payload.ID == sessionID {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a session event for %s", sessionID)
		}
	}
}

// requireConnectionEvent is requireSessionEvent's counterpart for the
// client's own ConnectionLost/ConnectionRecovered/ConnectionResync
// events (workspace.ConnectionEvent).
func requireConnectionEvent(t *testing.T, events <-chan any, state workspace.ConnectionState, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-events:
			if ev, ok := msg.(pubsub.Event[workspace.ConnectionEvent]); ok && ev.Payload.State == state {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s connection event", state)
		}
	}
}

// TestConnectRemoteWorkspace_ReconnectsAfterProcessKill covers CLIENT-
// SERVER.md's PR 3.1 reconnect test: killing the fake ssh process mid-
// session produces ConnectionLost, SSHDialer starts a NEW fake ssh
// process for the reconnect (spawn count goes up), ConnectionRecovered
// follows, and a session created on the daemon while the client's stream
// was down (through an independent, direct connection -- simulating
// something happening on the daemon during the gap) is still delivered
// exactly once via the resumed stream's replay-by-sequence-number, not
// lost and not duplicated.
func TestConnectRemoteWorkspace_ReconnectsAfterProcessKill(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	socketPath, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndRequestShutdown(t, context.Background(), socketPath) })

	tracker := &sshSpawnTracker{}
	target := transport.Target{Host: "fake-remote-host", Path: projectDir}
	dialerOpts := transport.DialerOptions{Command: tracker.command(socketPath)}

	client, _, cleanup, err := connectRemoteWorkspace(ctx, target, dialerOpts, "", false)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	events := make(chan any, 64)
	stop := client.SubscribeWith(func(msg any) { events <- msg })
	defer stop()

	before, err := client.CreateSession(ctx, "before-kill")
	require.NoError(t, err)
	requireSessionEvent(t, events, before.ID, daemonTestTimeout)
	require.Equal(t, 1, tracker.count(), "only the initial ssh process should exist so far")

	tracker.killLatest(t, daemonTestTimeout)
	requireConnectionEvent(t, events, workspace.ConnectionLost, daemonTestTimeout)

	// Publish an event on the daemon through an INDEPENDENT connection
	// while the main client's stream is down -- proves the reconnect
	// replays it by sequence number rather than needing the client to
	// have been live at the moment it happened.
	sideConn, err := supervisor.Dial(socketPath)
	require.NoError(t, err)
	sideClient := grpcws.NewClient(sideConn)
	duringGap, err := sideClient.CreateSession(ctx, "during-gap")
	require.NoError(t, err)
	sideClient.Shutdown()
	require.NoError(t, sideConn.Close())

	requireConnectionEvent(t, events, workspace.ConnectionRecovered, daemonTestTimeout)
	requireSessionEvent(t, events, duringGap.ID, daemonTestTimeout)

	require.GreaterOrEqual(t, tracker.count(), 2, "the dialer must have started a fresh ssh process to reconnect")

	// No duplicates: give any redelivery a moment to arrive, then drain
	// whatever is buffered and make sure duringGap's event shows up at
	// most once in total (requireSessionEvent above already consumed
	// its first occurrence).
	time.Sleep(200 * time.Millisecond)
	extra := 0
drain:
	for {
		select {
		case msg := <-events:
			if ev, ok := msg.(pubsub.Event[session.Session]); ok && ev.Payload.ID == duringGap.ID {
				extra++
			}
		default:
			break drain
		}
	}
	require.Zero(t, extra, "the during-gap session event must not be delivered a second time")
}

// TestSSHDialer_IdleConnSurvivesKeepaliveInterval checks that a healthy,
// idle connection through SSHDialer is not itself torn down by this
// package's own SetDeadline handling: gRPC's client keepalive pings an
// idle connection on a timer of its own (plain Writes, not
// conn.SetDeadline -- confirmed against grpc-go's http2_client.go, whose
// only SetDeadline caller is Close tearing the transport down, never
// keepalive), so nothing here should ever arm a deadline against a
// connection that is simply sitting idle between calls; a bug that did
// would make an idle attach drop every few keepalive intervals.
func TestSSHDialer_IdleConnSurvivesKeepaliveInterval(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	socketPath, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndRequestShutdown(t, context.Background(), socketPath) })

	tracker := &sshSpawnTracker{}
	target := transport.Target{Host: "fake-remote-host", Path: projectDir}

	const pingTime = 100 * time.Millisecond
	const pingTimeout = 100 * time.Millisecond
	dialOpts := append(grpcws.ClientDialOptions(pingTime, pingTimeout),
		grpc.WithContextDialer(transport.SSHDialer(target, transport.DialerOptions{Command: tracker.command(socketPath)})),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///"+target.String(), dialOpts...)
	require.NoError(t, err)
	defer conn.Close()

	client := grpcws.NewClient(conn)
	defer client.Shutdown()

	_, err = client.Hello(ctx)
	require.NoError(t, err)

	// Idle across several keepalive ping intervals -- long enough that a
	// spuriously fired deadline (this test's whole point) would already
	// have closed the connection.
	time.Sleep(8 * pingTime)

	_, err = client.Hello(ctx)
	require.NoError(t, err, "connection must survive several idle keepalive intervals")
	require.Equal(t, 1, tracker.count(), "no reconnect should have happened on a healthy idle connection")
}
