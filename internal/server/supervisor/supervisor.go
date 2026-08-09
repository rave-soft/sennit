// Package supervisor auto-starts and supervises the detached Braid server
// process on behalf of a client. It owns stale-socket detection, the
// single-flight spawn-and-wait-for-readiness sequence, and the health
// probing used to decide when a server is ready to serve requests.
//
// This package must not import cobra: it is invoked by internal/cmd but
// has no notion of flags or commands of its own.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/rave-soft/braid/internal/client"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/hostaddr"
	"github.com/rave-soft/braid/internal/lock"
	braidlog "github.com/rave-soft/braid/internal/log"
	"github.com/rave-soft/braid/internal/server"
	"github.com/rave-soft/braid/internal/version"
)

// Options carries the client-side settings the supervisor needs but has
// no way of discovering on its own (they come from cobra flags in the
// caller).
type Options struct {
	// Debug enables debug-level logging in the per-host server log file.
	Debug bool
	// ClientHost is the raw --host flag value the client was invoked
	// with. It is forwarded to a spawned server (via "server --host
	// ...") whenever it differs from hostaddr.DefaultHost(), so the
	// spawned server binds to the same address the client resolved.
	ClientHost string
}

// EnsureRunning auto-starts a detached server if the socket file does not
// exist. When the socket exists, it verifies that the running server
// version matches the client; on mismatch it shuts down the old server
// and starts a fresh one.
func EnsureRunning(ctx context.Context, hostURL *url.URL, opts Options) error {
	// Initialize the persistent log here so stale-socket diagnostics
	// emitted before connectToServer runs are captured in the per-host
	// server log file. braidlog.Setup uses sync.Once internally, so the
	// later call from connectToServer becomes a no-op.
	logFile := filepath.Join(config.GlobalCacheDir(), "server-"+SafeHostName(hostURL), "braid.log")
	braidlog.Setup(logFile, opts.Debug)

	switch hostURL.Scheme {
	case "unix", "npipe":
		needsStart := false
		_, statErr := os.Stat(hostURL.Host)
		switch {
		case statErr == nil:
			// Probe the socket explicitly before the version-check
			// path. A stale unix socket file (the previous server
			// exited without cleaning up) would otherwise make
			// restartIfStale spin on a non-responsive endpoint; here
			// we detect it with a short DialTimeout and remove the
			// orphaned file so the normal spawn path can run.
			if hostURL.Scheme == "unix" {
				conn, dialErr := net.DialTimeout( //nolint:noctx
					hostURL.Scheme, hostURL.Host, 200*time.Millisecond,
				)
				if dialErr == nil {
					conn.Close()
				} else if server.IsStaleSocketErr(dialErr) {
					slog.Warn("Stale socket detected, removing",
						"path", hostURL.Host, "error", dialErr)
					if err := os.Remove(hostURL.Host); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return fmt.Errorf("failed to remove stale server socket %q: %v", hostURL.Host, err)
					}
					needsStart = true
					break
				}
			}
			restarted, err := restartIfStale(ctx, hostURL)
			if err != nil {
				slog.Warn("Failed to check server version", "error", err)
			}
			needsStart = restarted || err != nil
		case errors.Is(statErr, fs.ErrNotExist):
			needsStart = true
		default:
			slog.Warn("Unexpected error stat'ing server socket, attempting cleanup",
				"path", hostURL.Host, "error", statErr)
			if err := os.Remove(hostURL.Host); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("failed to remove stale server socket %q: %v", hostURL.Host, err)
			}
			needsStart = true
		}

		if needsStart {
			if err := spawnAndWaitReady(ctx, hostURL, opts); err != nil {
				return fmt.Errorf("failed to initialize braid server: %v", err)
			}
			return nil
		}

		if err := waitForServerReady(ctx, hostURL); err != nil {
			return fmt.Errorf("failed to initialize braid server: %v", err)
		}
	}

	return nil
}

// Replace waits out the socket of a server that has committed to exiting,
// then brings up a fresh one.
func Replace(ctx context.Context, hostURL *url.URL, opts Options) error {
	if hostURL.Scheme == "unix" {
		if err := awaitSocketGone(ctx, hostURL); err != nil {
			return err
		}
	}
	if err := spawnAndWaitReady(ctx, hostURL, opts); err != nil {
		return fmt.Errorf("failed to initialize braid server: %v", err)
	}
	return nil
}

// spawnAndWaitReady serializes the spawn-and-wait-for-readiness sequence
// across concurrent clients via an exclusive flock on
// $XDG_CACHE_HOME/braid/server-<safeHost>/start.lock.
//
// After acquiring the lock it re-probes readiness so that a client that
// blocked while another client was spawning can skip its own spawn and
// just use the now-running server. The lock is held only for the
// duration of "spawn + readiness probe" and released before the caller
// resumes its normal lifetime.
func spawnAndWaitReady(ctx context.Context, hostURL *url.URL, opts Options) error {
	chDir, err := perHostServerDir(hostURL)
	if err != nil {
		return err
	}
	release, err := lock.File(ctx, filepath.Join(chDir, "start.lock"))
	if err != nil {
		// If the lock itself is unavailable, fall back to the
		// unsynchronized path rather than blocking the user.
		slog.Warn("Failed to acquire spawn lock, proceeding without single-flight", "error", err)
		if err := startDetachedServer(hostURL, opts); err != nil {
			return err
		}
		return waitForServerReady(ctx, hostURL)
	}
	defer release()

	// Another client may have just finished spawning while we were
	// waiting on the lock; if the server is already responsive, skip
	// the spawn entirely.
	probeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	probeErr := quickHealthProbe(probeCtx, hostURL)
	cancel()
	if probeErr == nil {
		return nil
	}

	if err := startDetachedServer(hostURL, opts); err != nil {
		return err
	}
	return waitForServerReady(ctx, hostURL)
}

// quickHealthProbe issues a single readiness request with the caller's
// context and returns nil iff the server is responsive right now.
func quickHealthProbe(ctx context.Context, hostURL *url.URL) error {
	httpClient, reqURL, err := readinessHTTPClient(hostURL)
	if err != nil {
		return err
	}
	return probeHealth(ctx, httpClient, reqURL, hostURL)
}

// perHostServerDir returns (and creates) the cache directory used for
// per-host server state (logs, start.lock, etc.). The path is derived
// from the parsed host URL rather than the global flag so the same key
// is computed regardless of where the host came from.
func perHostServerDir(hostURL *url.URL) (string, error) {
	chDir := filepath.Join(config.GlobalCacheDir(), "server-"+SafeHostName(hostURL))
	if err := os.MkdirAll(chDir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create server working directory: %v", err)
	}
	return chDir, nil
}

var safeNameRegexp = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// SafeHostName returns a filesystem-safe identifier for hostURL, suitable
// for use as a directory name. It mirrors the input shape of the --host
// flag so client and server compute the same key.
func SafeHostName(hostURL *url.URL) string {
	return safeNameRegexp.ReplaceAllString(
		hostURL.Scheme+"://"+hostURL.Host+hostURL.Path, "_",
	)
}

// serverReadyTimeout returns the total budget for the readiness probe.
// Overridable via BRAID_SERVER_READY_TIMEOUT (parsed as a Go duration).
func serverReadyTimeout() time.Duration {
	const def = 10 * time.Second
	v := os.Getenv("BRAID_SERVER_READY_TIMEOUT")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// waitForServerReady polls GET /v1/health until the server responds with
// any 2xx status or the total timeout elapses. Each attempt uses a short
// per-attempt timeout so a hung listener doesn't burn the whole budget.
//
// The HTTP transport is built to mirror how *client.Client dials so the
// same unix socket / npipe / tcp setups all work uniformly here.
func waitForServerReady(ctx context.Context, hostURL *url.URL) error {
	httpClient, reqURL, err := readinessHTTPClient(hostURL)
	if err != nil {
		return err
	}

	const perAttempt = 100 * time.Millisecond
	deadline := time.Now().Add(serverReadyTimeout())

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("timed out waiting for server readiness")
		}

		attemptCtx, cancel := context.WithTimeout(ctx, perAttempt)
		err := probeHealth(attemptCtx, httpClient, reqURL, hostURL)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(perAttempt):
		}
	}
}

// readinessHTTPClient builds an *http.Client whose transport dials the
// server using the same scheme-aware logic as *client.Client (unix
// socket, named pipe, or tcp).
func readinessHTTPClient(hostURL *url.URL) (*http.Client, string, error) {
	c, err := client.NewClient("", hostURL.Scheme, hostURL.Host)
	if err != nil {
		return nil, "", err
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return c.Dial(ctx, network, addr)
	}
	if hostURL.Scheme == "unix" || hostURL.Scheme == "npipe" {
		tr.DisableCompression = true
	}

	httpClient := &http.Client{Transport: tr}

	// For unix sockets / named pipes we still need a syntactically valid
	// HTTP URL; the actual address is resolved by the dialer.
	host := hostURL.Host
	if hostURL.Scheme == "unix" || hostURL.Scheme == "npipe" {
		host = client.DummyHost
	}
	reqURL := (&url.URL{Scheme: "http", Host: host, Path: "/v1/health"}).String()
	return httpClient, reqURL, nil
}

// probeHealth issues a single GET to the readiness endpoint and treats
// any 2xx response as success.
func probeHealth(ctx context.Context, h *http.Client, reqURL string, hostURL *url.URL) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if hostURL.Scheme == "unix" || hostURL.Scheme == "npipe" {
		req.Host = client.DummyHost
	}
	rsp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	_, _ = io.Copy(io.Discard, rsp.Body)
	if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
		return fmt.Errorf("server health check failed: %s", rsp.Status)
	}
	return nil
}

// restartIfStale checks whether the running server matches the current
// client version. When they differ it asks the server to stand down and,
// if it agrees, removes the stale socket so the caller can start a fresh
// server.
//
// The request is conditional and the server has the last word: it refuses
// while it is hosting anything, because BuildID derives from the
// executable's mtime, so any rebuild (including every `go run`) makes a
// second session look like an upgrade and would otherwise kill the first
// session's workspaces. Servers too old to understand the conditional
// command are left running for the same reason — the request they do
// understand is unconditional. They shut themselves down when they go
// idle, and the next client then finds no socket and spawns a current one.
//
// It returns restarted=true only when the server accepted the shutdown and
// the caller must spawn a replacement.
func restartIfStale(ctx context.Context, hostURL *url.URL) (restarted bool, err error) {
	c, err := client.NewClient("", hostURL.Scheme, hostURL.Host)
	if err != nil {
		return false, err
	}
	vi, err := c.VersionInfo(ctx)
	if err != nil {
		return false, err
	}
	if vi.Version == version.Version && vi.BuildID == version.BuildID {
		return false, nil
	}
	versionFields := []any{
		"server_version", vi.Version,
		"client_version", version.Version,
		"server_build_id", vi.BuildID,
		"client_build_id", version.BuildID,
	}
	// Every refusal — in use, too old to be asked, or unreachable — leads to
	// the same safe outcome: keep using the running server. The wrapped
	// error says which it was.
	if err := c.ShutdownServerIfIdle(ctx); err != nil {
		if !errors.Is(err, client.ErrUnsupported) {
			slog.Warn("Server version differs but it will not stand down; reusing it",
				append(versionFields, "error", err)...)
			return false, nil
		}
		// The server predates shutdown_if_idle. Fall back to the
		// unconditional command, but only after verifying it is idle.
		if !shutdownLegacyStaleServer(ctx, c, versionFields) {
			return false, nil
		}
	}
	slog.Info("Stale server accepted shutdown, restarting", versionFields...)
	if err := awaitSocketGone(ctx, hostURL); err != nil {
		return true, err
	}
	return true, nil
}

// shutdownLegacyStaleServer handles a stale server too old to understand the
// idle-checked shutdown. That server's only "shutdown" command is
// unconditional and would take live sessions down, so it is used only after
// listing workspaces confirms the server is idle. It reports whether the
// server accepted the shutdown; every other outcome (unreachable, busy, or a
// refused shutdown) is logged and reported as false so the caller reuses the
// running server.
func shutdownLegacyStaleServer(ctx context.Context, c *client.Client, versionFields []any) bool {
	workspaces, err := c.ListWorkspaces(ctx)
	if err != nil {
		slog.Warn("Server version differs but it will not stand down; reusing it",
			append(versionFields, "list_error", err)...)
		return false
	}
	if len(workspaces) > 0 {
		slog.Warn("Server version differs and has active workspaces; reusing it",
			append(versionFields, "workspaces", len(workspaces))...)
		return false
	}
	if err := c.ShutdownServer(ctx); err != nil {
		slog.Warn("Server version differs but it will not stand down; reusing it",
			append(versionFields, "error", err)...)
		return false
	}
	return true
}

// awaitSocketGone gives a server that has committed to exiting a moment to
// release its socket, then force-removes whatever is left: the old process
// has latched its decision and will not serve requests again.
func awaitSocketGone(ctx context.Context, hostURL *url.URL) error {
	for range 20 {
		if _, err := os.Stat(hostURL.Host); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = os.Remove(hostURL.Host)
	return nil
}

func startDetachedServer(hostURL *url.URL, opts Options) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	chDir, err := perHostServerDir(hostURL)
	if err != nil {
		return err
	}

	cmdArgs := []string{"server"}
	if opts.ClientHost != hostaddr.DefaultHost() {
		cmdArgs = append(cmdArgs, "--host", opts.ClientHost)
	}

	// Use context.Background() so the parent's context cancellation does not
	// kill the spawned server. detachProcess (Setsid on !windows,
	// DETACHED_PROCESS on windows) is what truly detaches the child from
	// this process's lifetime.
	c := exec.CommandContext(context.Background(), exe, cmdArgs...)
	stdoutPath := filepath.Join(chDir, "stdout.log")
	stderrPath := filepath.Join(chDir, "stderr.log")
	detachProcess(c)

	stdout, err := os.Create(stdoutPath)
	if err != nil {
		return fmt.Errorf("failed to create stdout log file: %v", err)
	}
	defer stdout.Close()
	c.Stdout = stdout

	stderr, err := os.Create(stderrPath)
	if err != nil {
		return fmt.Errorf("failed to create stderr log file: %v", err)
	}
	defer stderr.Close()
	c.Stderr = stderr

	if err := c.Start(); err != nil {
		return fmt.Errorf("failed to start braid server: %v", err)
	}

	if err := c.Process.Release(); err != nil {
		return fmt.Errorf("failed to detach braid server process: %v", err)
	}

	return nil
}
