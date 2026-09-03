package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/stretchr/testify/require"
)

// These tests target Registry's public orchestration surface — Initialize,
// CatalogSnapshot, GetStates, Version, Tools, RunTool, RefreshTools, and
// Close racing an in-flight connection — which is where the 12
// synchronization primitives interact and where a lock-ordering mistake
// would actually surface. They deliberately avoid spawning any real MCP
// server process (see the package's existing liveSession helper in
// lifecycle_test.go): a "connected" server is simulated by driving the real
// production registration path (beginAttempt + publishSession) against an
// in-memory MCP session, and a "server that fails to start" uses a stdio
// command that cannot possibly exist so createSession fails fast and
// deterministically instead of depending on what's installed on the host.

// seedConnected drives the real publishSession path against an in-memory
// session, exactly as a successful createSession would, so tests can assert
// against a "connected" server without spawning a subprocess or a real
// network transport.
func seedConnected(t *testing.T, r *Registry, name string, m config.MCPConfig) {
	t.Helper()
	sess := liveSessionWithCapabilities(t, "do_thing", "a_prompt", "res://a")
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	require.NoError(t, r.publishSession(context.Background(), name, m, owner, sess))
}

func TestInitialize_ConnectsAndPopulatesCatalog(t *testing.T) {
	// Initialize's own contribution is the per-server orchestration loop
	// (disabled-skip, fan-out, wg.Wait), not connection establishment
	// itself — that is exercised in depth elsewhere via createSession and
	// publishSession. So this pins the loop: a disabled server is left
	// disabled and never attempted, and CatalogSnapshot/GetStates/Version
	// observe whatever a connected server publishes, once Initialize
	// returns (which is only after every attempt has settled).
	const connected = "init-connected"
	const disabled = "init-disabled"
	r := NewRegistry()
	r.ArmInit()

	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{
		disabled: {Type: config.MCPStdio, Disabled: true},
	}})

	before := r.Version()
	r.Initialize(context.Background(), nil, cfg)

	info, ok := r.GetState(disabled)
	require.True(t, ok)
	require.Equal(t, StateDisabled, info.State)

	// Simulate what a real successful connect would have published for a
	// second, enabled server (see seedConnected's doc for why).
	seedConnected(t, r, connected, config.MCPConfig{Type: config.MCPStdio})

	snap := r.CatalogSnapshot()
	require.Contains(t, snap.Tools, connected)
	require.Len(t, snap.Tools[connected], 1)
	require.Contains(t, snap.Prompts, connected)
	require.Contains(t, snap.Resources, connected)
	require.Greater(t, snap.Version, before)

	states := r.GetStates()
	require.Equal(t, StateConnected, states[connected].State)
	require.Equal(t, StateDisabled, states[disabled].State)

	tools := map[string][]*Tool{}
	for name, ts := range r.Tools() {
		tools[name] = ts
	}
	require.Contains(t, tools, connected)

	require.NoError(t, r.Close(context.Background()))
}

func TestInitialize_ServerFailsToStart(t *testing.T) {
	// A command that cannot possibly exist makes createSession fail during
	// exec, deterministically and fast, without depending on any MCP
	// server being installed on the test host.
	const name = "init-fails-to-start"
	r := NewRegistry()
	r.ArmInit()

	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "/no/such/binary-sennit-test-xyz"},
	}})

	r.Initialize(context.Background(), nil, cfg)

	info, ok := r.GetState(name)
	require.True(t, ok)
	require.Equal(t, StateError, info.State)
	require.Error(t, info.Error)

	_, ok = r.CatalogSnapshot().Tools[name]
	require.False(t, ok, "a server that failed to start must not advertise tools")

	require.NoError(t, r.Close(context.Background()))
}

func TestInitializeSingle_UnknownServer(t *testing.T) {
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{})
	err := r.InitializeSingle(context.Background(), "does-not-exist", cfg)
	require.Error(t, err)
}

func TestRunTool_CallsToolOnConnectedSession(t *testing.T) {
	const name = "runtool-server"
	r := NewRegistry()
	r.ping = func(context.Context, *ClientSession, time.Duration) error { return nil }
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	seedConnected(t, r, name, config.MCPConfig{Type: config.MCPStdio})

	result, err := r.RunTool(context.Background(), cfg, name, "do_thing", `{}`)
	require.NoError(t, err)
	require.Equal(t, "text", result.Type)
	require.Equal(t, "ok", result.Content)
}

func TestRunTool_InvalidJSONInput(t *testing.T) {
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{})
	_, err := r.RunTool(context.Background(), cfg, "whatever", "tool", `not json`)
	require.Error(t, err)
}

func TestRefreshTools_UpdatesCatalogAndCounts(t *testing.T) {
	const name = "refresh-server"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	seedConnected(t, r, name, config.MCPConfig{Type: config.MCPStdio})

	before := r.Version()
	r.RefreshTools(context.Background(), cfg, name)

	info, ok := r.GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, info.State)
	require.Equal(t, 1, info.Counts.Tools)
	require.Greater(t, r.Version(), before)
}

func TestRefreshTools_NoSessionIsNoop(t *testing.T) {
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{})
	// Must not panic or hang when nothing is connected.
	r.RefreshTools(context.Background(), cfg, "nothing-here")
}

// TestClose_WhileConnectionInFlight pins Close's shutdown ordering against a
// connection attempt that has announced itself (StateStarting, so it is
// captured by Close's teardown sweep) but not yet published: Close must
// invalidate the attempt's ownership (by bumping the server's generation)
// before the in-flight attempt can commit, and the attempt's publish must
// then discard its session instead of registering it, leaving Close free to
// return without waiting on a connection it doesn't know about yet.
func TestClose_WhileConnectionInFlight(t *testing.T) {
	const name = "close-in-flight"
	r := NewRegistry()
	r.ArmInit()

	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.updateStateFor(name, owner, StateStarting, nil, withPending(config.MCPConfig{Type: config.MCPStdio}))

	sess, sessCtx := liveSession(t, "tool")

	closeDone := make(chan struct{})
	publishDone := make(chan error, 1)
	go func() {
		<-closeDone
		publishDone <- r.publishOrClose(context.Background(), name, config.MCPConfig{Type: config.MCPStdio}, owner, sess)
	}()

	require.NoError(t, r.Close(context.Background()))
	close(closeDone)

	// The race is lost ownership (a newer generation, from Close's teardown
	// sweep), not caller cancellation - see G22: publishOrClose used to
	// return context.Canceled for this, which a model-facing caller
	// (mcp-tools.go) couldn't tell apart from real cancellation and so
	// aborted the whole tool-call batch over a condition the model could
	// just have retried.
	require.ErrorIs(t, <-publishDone, errLostOwnership)
	require.ErrorIs(t, sessCtx.Err(), context.Canceled,
		"a connection that loses the race with Close must be closed, not leaked")

	_, ok := r.sessions.Get(name)
	require.False(t, ok, "a connection that raced with Close must never be registered")
}

func TestReinitialize_RemovesServerGoneFromConfig(t *testing.T) {
	const name = "reinit-removed"
	r := NewRegistry()
	seedConnected(t, r, name, config.MCPConfig{Type: config.MCPStdio})
	cfg := configtest.NewStore(t, &config.Config{}) // the server no longer exists in config

	r.Reinitialize(context.Background(), cfg)

	_, ok := r.GetState(name)
	require.False(t, ok, "a server removed from config must have its state entry removed")
}

func TestReinitialize_StartsNewlyAddedServer(t *testing.T) {
	// reconcileOnce's reinitStart path launches goInitClient asynchronously
	// (Reinitialize does not wait for it, unlike Initialize), so this
	// subscribes to events rather than asserting state immediately after
	// Reinitialize returns.
	const name = "reinit-new-fails"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "/no/such/binary-sennit-test-xyz"},
	}})
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub := r.SubscribeEvents(subCtx)

	r.Reinitialize(context.Background(), cfg)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sub:
			if ev.Payload.Name == name && ev.Payload.Type == EventStateChanged && ev.Payload.State == StateError {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the newly-added server to settle in StateError")
		}
	}
}

func TestPendingAuthMCPs_ListsServersNeedingAuth(t *testing.T) {
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{
		"needs-auth": {Type: config.MCPHttp, URL: "https://example.com/mcp", OAuth: true},
	}})
	r.updateState("needs-auth", StateNeedsAuth, nil, nil, Counts{})

	pending := r.PendingAuthMCPs(cfg)
	require.Len(t, pending, 1)
	require.Equal(t, "needs-auth", pending[0].Name)
	require.Equal(t, "https://example.com/mcp", pending[0].URL)

	require.Empty(t, r.MCPAuthURL("needs-auth"), "no auth handler has been published for this server yet")
}

// TestOwns_RaceAgainstBeginAttempt hammers r.owns (a read of the plain
// r.owners map) concurrently with r.beginAttempt (a write to the same map)
// for the same server name. r.owners is an ordinary Go map, not a csync.Map,
// so it has no built-in synchronization of its own: publishMu is the only
// thing standing between this and a runtime-fatal concurrent map read/write.
// This is also the mutation-test harness for that claim (see the report):
// with the lock in owns() removed, this test fails under -race.
func TestOwns_RaceAgainstBeginAttempt(t *testing.T) {
	const name = "owns-race"
	r := NewRegistry()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.beginAttempt(name)
		}()
		go func() {
			defer wg.Done()
			_ = r.owns(name, attemptID{gen: 0, seq: 1})
		}()
	}
	wg.Wait()
}

// TestRegistry_ConcurrentAccessRace hammers the public read/write surface
// concurrently — RunTool, Tools, GetStates, CatalogSnapshot, RefreshTools —
// against one connected server. It asserts nothing about outcomes beyond "no
// panic"; its job is to give `-race` many chances to catch a primitive that
// is missing a lock or a data structure two of these paths touch without the
// same lock.
func TestRegistry_ConcurrentAccessRace(t *testing.T) {
	const name = "race-server"
	r := NewRegistry()
	r.ping = func(context.Context, *ClientSession, time.Duration) error { return nil }
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	seedConnected(t, r, name, config.MCPConfig{Type: config.MCPStdio})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(5)
		go func() {
			defer wg.Done()
			_, _ = r.RunTool(context.Background(), cfg, name, "do_thing", `{}`)
		}()
		go func() {
			defer wg.Done()
			for range r.Tools() {
			}
		}()
		go func() {
			defer wg.Done()
			_ = r.GetStates()
		}()
		go func() {
			defer wg.Done()
			_ = r.CatalogSnapshot()
		}()
		go func() {
			defer wg.Done()
			r.RefreshTools(context.Background(), cfg, name)
		}()
	}
	wg.Wait()

	require.NoError(t, r.Close(context.Background()))
}
