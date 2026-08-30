package mcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/stretchr/testify/require"
)

// liveSession spins up a real in-memory MCP server exposing a single tool and
// returns a connected client session wrapped as a *ClientSession, mirroring
// what defaultRegistry.createSession produces in production. The returned context is the one
// bound to the session's cancel func, so a test can assert the session was
// actually closed (ctx cancelled) rather than merely dropped. Both sides are
// torn down via t.Cleanup.
func liveSession(t *testing.T, toolName string) (*ClientSession, context.Context) {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "srv"}, nil)
	mcp.AddTool(
		server,
		&mcp.Tool{Name: toolName, Description: "test tool"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		},
	)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	client := mcp.NewClient(&mcp.Implementation{Name: "sennit-test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	return &ClientSession{ClientSession: clientSession, cancel: cancel}, ctx
}

// liveSessionWithCapabilities is like liveSession but the server also exposes a
// prompt and a resource, so tests can assert those registries are populated on
// (re)connect.
func liveSessionWithCapabilities(t *testing.T, toolName, promptName, resourceURI string) *ClientSession {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "srv"}, nil)
	mcp.AddTool(
		server,
		&mcp.Tool{Name: toolName, Description: "test tool"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		},
	)
	server.AddPrompt(
		&mcp.Prompt{Name: promptName},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{}, nil
		},
	)
	server.AddResource(
		&mcp.Resource{Name: "res", URI: resourceURI},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{}, nil
		},
	)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	client := mcp.NewClient(&mcp.Implementation{Name: "sennit-test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	return &ClientSession{ClientSession: clientSession, cancel: cancel}
}

// TestUpdateState_ErrorClosesSessionAndClearsTools pins the primary fix: a
// StateError transition must (1) remove the session from the map, (2) actually
// close it so its child process/pipes are released, and (3) clear its tools
// from the registry. Before the fix defaultRegistry.updateState only did a bare
// defaultRegistry.sessions.Del(name): the session was leaked and its tools lingered, so
// sennit_info kept reading "connected, N tools" while the LLM's tool list and
// the live session had diverged.
func TestUpdateState_ErrorClosesSessionAndClearsTools(t *testing.T) {
	const name = "test-error-cleanup"
	t.Cleanup(func() {
		defaultRegistry.sessions.Del(name)
		defaultRegistry.allTools.Del(name)
		defaultRegistry.states.Del(name)
	})

	sess, sessCtx := liveSession(t, "do_thing")
	defaultRegistry.sessions.Set(name, sess)
	defaultRegistry.allTools.Set(name, []*Tool{{Name: "do_thing"}})

	// Preconditions: tool registered and session live.
	_, ok := defaultRegistry.allTools.Get(name)
	require.True(t, ok)
	require.NoError(t, sessCtx.Err(), "session context must be live before the error")

	defaultRegistry.updateState(name, StateError, errors.New("stdio pipe broke"), nil, Counts{Tools: 1})

	// The dead session is removed from the map...
	_, ok = defaultRegistry.sessions.Get(name)
	require.False(t, ok, "errored session must be removed from the defaultRegistry.sessions map")

	// ...actually closed (its context is cancelled, not merely dropped)...
	require.ErrorIs(t, sessCtx.Err(), context.Canceled, "errored session must be closed, not just dropped from the map")

	// ...and its tools cleared from the registry the agent sends to the LLM.
	_, ok = defaultRegistry.allTools.Get(name)
	require.False(t, ok, "errored session's tools must be cleared from the registry")

	info, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateError, info.State)
}

// TestUpdateState_ConfigBookkeeping pins the config snapshot reconcile relies
// on: StateConnected records the config now in effect and clears any pending
// attempt, StateStarting records the config the in-flight attempt is using,
// StateDisabled clears the recorded config so a re-enable restarts, and every
// other transition preserves what was there.
func TestUpdateState_ErrorDetachesBeforeClosingOutsidePublishLock(t *testing.T) {
	const name = "test-error-close-order"
	r := NewRegistry()
	session, _ := liveSession(t, "do_thing")
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	session.closeIdle = func() {
		close(closeStarted)
		<-releaseClose
	}
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, session)
	r.sessionOwners[name] = owner
	r.allTools.Set(name, []*Tool{{Name: "do_thing"}})
	r.publishMu.Unlock()

	updated := make(chan struct{})
	go func() {
		r.updateStateFor(name, owner, StateError, errors.New("broken"))
		close(updated)
	}()
	<-closeStarted

	require.True(t, r.publishMu.TryLock(), "session close must run after releasing publishMu")
	_, hasSession := r.sessions.Get(name)
	_, hasOwner := r.owners[name]
	_, hasSessionOwner := r.sessionOwners[name]
	_, hasTools := r.allTools.Get(name)
	info, hasState := r.states.Get(name)
	r.publishMu.Unlock()
	require.False(t, hasSession)
	require.False(t, hasOwner)
	require.False(t, hasSessionOwner)
	require.False(t, hasTools)
	require.True(t, hasState)
	require.Equal(t, StateError, info.State)

	close(releaseClose)
	<-updated
}

func TestUpdateState_ConfigBookkeeping(t *testing.T) {
	const name = "test-config-bookkeeping"
	t.Cleanup(func() {
		defaultRegistry.states.Del(name)
	})

	base := config.MCPConfig{Type: config.MCPHttp, URL: "https://example.com/mcp"}
	changed := base
	changed.URL = "https://other.com/mcp"

	// Connecting records the config and clears any pending attempt.
	defaultRegistry.updateState(name, StateStarting, nil, nil, Counts{}, withPending(base))
	defaultRegistry.updateState(name, StateConnected, nil, nil, Counts{}, withConfig(base))
	info, _ := GetState(name)
	require.Equal(t, base, info.Config, "connected state must record its config")
	require.Nil(t, info.PendingConfig, "connected state must clear the pending config")

	// Starting records the config the attempt is connecting with.
	defaultRegistry.updateState(name, StateStarting, nil, nil, Counts{}, withPending(changed))
	info, _ = GetState(name)
	require.NotNil(t, info.PendingConfig, "starting state must record the pending config")
	require.Equal(t, changed, *info.PendingConfig)
	require.Equal(t, base, info.Config, "starting must not disturb the last connected config")

	// An error preserves both so reconcile can still reason about the server.
	defaultRegistry.updateState(name, StateError, errors.New("boom"), nil, Counts{})
	info, _ = GetState(name)
	require.Equal(t, base, info.Config, "error must preserve the connected config")
	require.NotNil(t, info.PendingConfig, "error must preserve the pending config")

	// Disabling clears both so a re-enable with an unchanged config restarts.
	defaultRegistry.updateState(name, StateDisabled, nil, nil, Counts{})
	info, _ = GetState(name)
	require.Equal(t, config.MCPConfig{}, info.Config, "disabled must clear the connected config")
	require.Nil(t, info.PendingConfig, "disabled must clear the pending config")
}

// TestUpdateState_ErrorClearsPromptsAndResources pins that a StateError
// transition also drops the dead server's prompts and resources, not just its
// tools. Leaving them registered lets a disconnected server keep advertising
// capabilities the agent can no longer fulfil — the same state/registry
// divergence the tool clear exists to prevent.
func TestUpdateState_ErrorClearsPromptsAndResources(t *testing.T) {
	const name = "test-error-clears-all"
	t.Cleanup(func() {
		defaultRegistry.sessions.Del(name)
		defaultRegistry.allTools.Del(name)
		defaultRegistry.allPrompts.Del(name)
		defaultRegistry.allResources.Del(name)
		defaultRegistry.states.Del(name)
	})

	defaultRegistry.allTools.Set(name, []*Tool{{Name: "do_thing"}})
	defaultRegistry.allPrompts.Set(name, []*Prompt{{Name: "a_prompt"}})
	defaultRegistry.allResources.Set(name, []*Resource{{Name: "a_resource"}})

	defaultRegistry.updateState(name, StateError, errors.New("pipe broke"), nil, Counts{})

	_, ok := defaultRegistry.allTools.Get(name)
	require.False(t, ok, "errored session's tools must be cleared")
	_, ok = defaultRegistry.allPrompts.Get(name)
	require.False(t, ok, "errored session's prompts must be cleared")
	_, ok = defaultRegistry.allResources.Get(name)
	require.False(t, ok, "errored session's resources must be cleared")
}

// TestGetOrRenewClient_StalePingCannotReplaceNewSession pins the
// re-check-before-renew path: a caller that started pinging an old session
// before it was superseded by a fresh one must re-ping the current session
// rather than blindly rebuilding over it. Here the fresh session's own ping
// also fails (via the mock), which is a genuine connection failure, not the
// stale caller merely losing a race to someone else's healthy session — it
// must surface as errPingFailed and drive the server to StateError (which
// tears the failed session down), not be swallowed as a
// lost-ownership/cancellation condition that leaves a known-broken session
// sitting in the registry as if nothing happened.
func TestGetOrRenewClient_StalePingCannotReplaceNewSession(t *testing.T) {
	const name = "test-stale-ping"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	old, _ := liveSession(t, "old")
	oldOwner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, old)
	r.sessionOwners[name] = oldOwner
	r.publishMu.Unlock()

	pingStarted := make(chan struct{})
	releasePing := make(chan struct{})
	var pingCalls atomic.Int32
	r.ping = func(context.Context, *ClientSession, time.Duration) error {
		if pingCalls.Add(1) == 1 {
			close(pingStarted)
			<-releasePing
		}
		return errors.New("stale ping")
	}
	result := make(chan error, 1)
	go func() {
		_, err := r.getOrRenewClient(context.Background(), cfg, name)
		result <- err
	}()
	<-pingStarted

	r.teardown(name)
	fresh, freshCtx := liveSession(t, "fresh")
	freshOwner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, fresh)
	r.sessionOwners[name] = freshOwner
	r.publishMu.Unlock()
	close(releasePing)
	require.ErrorIs(t, <-result, errPingFailed)
	info, ok := r.states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateError, info.State)
	// StateError's cleanup takes and closes the failed session; it must not
	// be left behind as if the failure had never happened.
	_, ok = r.sessions.Get(name)
	require.False(t, ok, "failed session must not remain published")
	require.ErrorIs(t, freshCtx.Err(), context.Canceled, "failed session must be closed")
}

// TestGetOrRenewClient_CancelledContextTakesQuietPath is the counterpart to
// TestGetOrRenewClient_StalePingCannotReplaceNewSession: when the caller's
// own context is what caused the re-check ping to fail, that is genuine user
// cancellation, not a broken connection, and must still take the quiet path
// (propagate context.Canceled, leave the session alone) rather than being
// promoted to errPingFailed/StateError.
func TestGetOrRenewClient_CancelledContextTakesQuietPath(t *testing.T) {
	const name = "test-cancel-quiet"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	old, _ := liveSession(t, "old")
	oldOwner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, old)
	r.sessionOwners[name] = oldOwner
	r.publishMu.Unlock()

	fresh, freshCtx := liveSession(t, "fresh")
	t.Cleanup(func() { _ = fresh.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The first ping (against the old, observed session) simulates another
	// attempt taking over the server while it was in flight, so the second
	// ping below (under the renew lock) runs against a session this call
	// never observed, exercising the re-check branch.
	var pingCalls atomic.Int32
	r.ping = func(context.Context, *ClientSession, time.Duration) error {
		if pingCalls.Add(1) == 1 {
			r.teardown(name)
			freshOwner, err := r.beginAttempt(name)
			require.NoError(t, err)
			r.publishMu.Lock()
			r.sessions.Set(name, fresh)
			r.sessionOwners[name] = freshOwner
			r.publishMu.Unlock()
		}
		return context.Canceled
	}

	_, err = r.getOrRenewClient(ctx, cfg, name)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, errPingFailed)
	if info, ok := r.states.Get(name); ok {
		require.NotEqual(t, StateError, info.State, "genuine cancellation must not surface as a connection failure")
	}
	require.NoError(t, freshCtx.Err(), "cancellation must not tear down the session")
}

// TestGetOrRenewClient_CancelledContextOnFirstPingLeavesSessionAlone pins the
// common case the two tests above don't reach: a caller that's cancelled
// before its very first ping, against the session it still owns (no
// concurrent takeover in play). The `!observed || owner != observedOwner ||
// session != observedSession` guard in getOrRenewClient exists to turn a
// ctx-cancelled ping into quiet cancellation, but it only runs on the
// re-check path — when the session under the renew lock is the same one
// just pinged, that branch is skipped entirely, so without a check right
// after the first ping, execution fell through to beginRenewal and tore
// down a perfectly healthy session (killing a stdio server's process)
// just because the caller happened to be cancelled.
func TestGetOrRenewClient_CancelledContextOnFirstPingLeavesSessionAlone(t *testing.T) {
	const name = "test-cancel-first-ping"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	sess, sessCtx := liveSession(t, "do_thing")
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, sess)
	r.sessionOwners[name] = owner
	r.publishMu.Unlock()

	r.ping = func(ctx context.Context, _ *ClientSession, _ time.Duration) error {
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := r.getOrRenewClient(ctx, cfg, name)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, errPingFailed)
	require.Nil(t, got)

	// The session must be untouched: same one still published, still alive,
	// and the server must not have been driven into StateError.
	current, ok := r.sessions.Get(name)
	require.True(t, ok, "session must still be published")
	require.Same(t, sess, current, "session must not have been replaced")
	require.NoError(t, sessCtx.Err(), "cancellation must not tear down the session")
	if info, ok := r.states.Get(name); ok {
		require.NotEqual(t, StateError, info.State, "genuine cancellation must not surface as a connection failure")
	}
}

func TestGetOrRenewClient_SerializesConcurrentRenewals(t *testing.T) {
	const name = "test-renew-concurrency"
	const workers = 8

	t.Cleanup(func() {
		if s, ok := defaultRegistry.sessions.Take(name); ok {
			_ = s.Close()
		}
		defaultRegistry.allTools.Del(name)
		defaultRegistry.states.Del(name)
	})

	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	// Seed a dead session so the first ping fails and every worker attempts a
	// renewal.
	dead, _ := liveSession(t, "send_message")
	require.NoError(t, dead.Close())
	owner, err := defaultRegistry.beginAttempt(name)
	require.NoError(t, err)
	defaultRegistry.publishMu.Lock()
	defaultRegistry.sessions.Set(name, dead)
	defaultRegistry.sessionOwners[name] = owner
	defaultRegistry.publishMu.Unlock()

	// Pre-build enough live replacements that the buggy (unserialized) path
	// could consume more than one; the fix must consume exactly one.
	replacements := make(chan *ClientSession, workers)
	for range workers {
		s, _ := liveSession(t, "send_message")
		replacements <- s
	}
	close(replacements)
	t.Cleanup(func() {
		for s := range replacements {
			_ = s.Close()
		}
	})

	var created atomic.Int32
	origNewSession := defaultRegistry.newSession
	defaultRegistry.newSession = func(context.Context, ConfigProvider, string, config.MCPConfig, attemptID, config.VariableResolver, bool) (*ClientSession, error) {
		created.Add(1)
		return <-replacements, nil
	}
	t.Cleanup(func() { defaultRegistry.newSession = origNewSession })

	var wg sync.WaitGroup
	results := make([]*ClientSession, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			results[i], errs[i] = defaultRegistry.getOrRenewClient(context.Background(), cfg, name)
		})
	}
	wg.Wait()

	require.Equal(t, int32(1), created.Load(),
		"exactly one renewal must occur; concurrent callers must reuse the renewed session")

	final, ok := defaultRegistry.sessions.Get(name)
	require.True(t, ok, "a live session must remain registered after concurrent renewals")
	for i := range workers {
		require.NoError(t, errs[i])
		require.Same(t, final, results[i], "every caller must observe the same renewed session")
	}
}

// TestSessionErrorThenRenew_RestoresTools is the end-to-end regression for the
// reported bug: an MCP tool works, the stdio session drops mid-conversation,
// and afterwards every call returned "tool not found" forever. It walks the
// exact registry transitions the production code performs — initial connect
// registers tools, a StateError clears them (and closes the session), and the
// lazy renew re-registers them — so a regression in any leg (tools left stale
// on error, or tools never restored on renew) fails here.
func TestSessionErrorThenRenew_RestoresTools(t *testing.T) {
	const name = "test-error-then-renew"
	t.Cleanup(func() {
		if s, ok := defaultRegistry.sessions.Take(name); ok {
			_ = s.Close()
		}
		defaultRegistry.allTools.Del(name)
		defaultRegistry.states.Del(name)
	})

	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	// 1. Initial connect registers the tool via the live publishSession seam
	// (what defaultRegistry.initClient calls after establishing a session).
	sess1, _ := liveSession(t, "send_message")
	owner1, err := defaultRegistry.beginAttempt(name)
	require.NoError(t, err)
	err = defaultRegistry.publishSession(context.Background(), name, cfg.Config().MCP[name], owner1, sess1)
	require.NoError(t, err)
	_, ok := defaultRegistry.allTools.Get(name)
	require.True(t, ok, "tool should be registered after the initial connect")

	// 2. The session drops mid-conversation -> StateError. Post-fix this clears
	//    the tools and closes the dead session.
	defaultRegistry.updateState(name, StateError, errors.New("pipe broke"), nil, Counts{Tools: 1})
	_, ok = defaultRegistry.allTools.Get(name)
	require.False(t, ok, "tools must be cleared when the session errors")
	_, ok = defaultRegistry.sessions.Get(name)
	require.False(t, ok, "errored session must be removed from the map")

	// 3. The lazy renew path creates a fresh session and MUST re-register the
	//    tools. The bug was that it never did: the LLM's tool list stayed empty
	//    and every subsequent call returned "tool not found".
	sess2, _ := liveSession(t, "send_message")
	owner2, err := defaultRegistry.beginAttempt(name)
	require.NoError(t, err)
	err = defaultRegistry.publishSession(context.Background(), name, cfg.Config().MCP[name], owner2, sess2)
	require.NoError(t, err)

	got, ok := defaultRegistry.allTools.Get(name)
	require.True(t, ok, "tools must be restored after the session is renewed")
	require.Len(t, got, 1)
	require.Equal(t, "send_message", got[0].Name)
}

// TestGetOrRenewClient_RestoresPromptsAndResources pins that a renewal
// repopulates every registry and reports counts that match. StateError clears
// tools, prompts, and resources; if renewal restored only tools while keeping
// the old prompt/resource counts, GetState would again advertise capabilities
// absent from the registries.
func TestGetOrRenewClient_RestoresPromptsAndResources(t *testing.T) {
	const name = "test-renew-prompts-resources"
	t.Cleanup(func() {
		if s, ok := defaultRegistry.sessions.Take(name); ok {
			_ = s.Close()
		}
		defaultRegistry.allTools.Del(name)
		defaultRegistry.allPrompts.Del(name)
		defaultRegistry.allResources.Del(name)
		defaultRegistry.states.Del(name)
	})

	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	// Seed a dead session so the renewal path runs.
	dead, _ := liveSession(t, "send_message")
	require.NoError(t, dead.Close())
	owner, err := defaultRegistry.beginAttempt(name)
	require.NoError(t, err)
	defaultRegistry.publishMu.Lock()
	defaultRegistry.sessions.Set(name, dead)
	defaultRegistry.sessionOwners[name] = owner
	defaultRegistry.publishMu.Unlock()
	// Stale counts that must be recomputed, not preserved.
	defaultRegistry.updateState(name, StateConnected, nil, dead, Counts{Tools: 1, Prompts: 1, Resources: 1})

	replacement := liveSessionWithCapabilities(t, "send_message", "a_prompt", "res://thing")
	origNewSession := defaultRegistry.newSession
	defaultRegistry.newSession = func(context.Context, ConfigProvider, string, config.MCPConfig, attemptID, config.VariableResolver, bool) (*ClientSession, error) {
		return replacement, nil
	}
	t.Cleanup(func() { defaultRegistry.newSession = origNewSession })

	sess, err := defaultRegistry.getOrRenewClient(context.Background(), cfg, name)
	require.NoError(t, err)
	require.Same(t, replacement, sess)

	tools, ok := defaultRegistry.allTools.Get(name)
	require.True(t, ok, "tools must be restored on renewal")
	require.Len(t, tools, 1)

	prompts, ok := defaultRegistry.allPrompts.Get(name)
	require.True(t, ok, "prompts must be restored on renewal")
	require.Len(t, prompts, 1)

	resources, ok := defaultRegistry.allResources.Get(name)
	require.True(t, ok, "resources must be restored on renewal")
	require.Len(t, resources, 1)

	info, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, info.State)
	require.Equal(t, Counts{Tools: 1, Prompts: 1, Resources: 1}, info.Counts,
		"reported counts must match the restored registries")
}

// TestGetOrRenewClient_RenewalDetachesFromCallerContext is the regression
// test for the renewed-session-killed-on-tool-call-return bug: a lazy
// renewal (a broken ping discovered inside a tool call) used to build the
// new session straight off the caller's ctx, which createSession derives
// its stdio transport's exec.CommandContext and SIGKILL-the-group
// cmd.Cancel from. The tool call's own ctx is cancelled the moment the
// tool call returns, killing the freshly spawned server immediately - the
// next call would ping it, find it dead, and renew again, paying process
// start + initialize + list-tools on every single call. The fix builds the
// renewed session off context.WithoutCancel(ctx) instead, so the session
// outlives the tool call that happened to trigger the renewal.
func TestGetOrRenewClient_RenewalDetachesFromCallerContext(t *testing.T) {
	const name = "test-renew-detached-ctx"
	t.Cleanup(func() {
		if s, ok := defaultRegistry.sessions.Take(name); ok {
			_ = s.Close()
		}
		defaultRegistry.allTools.Del(name)
		defaultRegistry.states.Del(name)
	})

	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	// Seed a dead session so the renewal path runs.
	dead, _ := liveSession(t, "send_message")
	require.NoError(t, dead.Close())
	owner, err := defaultRegistry.beginAttempt(name)
	require.NoError(t, err)
	defaultRegistry.publishMu.Lock()
	defaultRegistry.sessions.Set(name, dead)
	defaultRegistry.sessionOwners[name] = owner
	defaultRegistry.publishMu.Unlock()

	replacement, _ := liveSession(t, "send_message")
	t.Cleanup(func() { _ = replacement.Close() })

	var capturedCtx context.Context
	origNewSession := defaultRegistry.newSession
	defaultRegistry.newSession = func(ctx context.Context, _ ConfigProvider, _ string, _ config.MCPConfig, _ attemptID, _ config.VariableResolver, _ bool) (*ClientSession, error) {
		capturedCtx = ctx
		return replacement, nil
	}
	t.Cleanup(func() { defaultRegistry.newSession = origNewSession })

	// Simulate the tool call's own ctx: it is what a lazy renewal is
	// triggered under (mcp-tools.go), and it is cancelled the moment that
	// tool call returns.
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	sess, err := defaultRegistry.getOrRenewClient(callerCtx, cfg, name)
	require.NoError(t, err)
	require.Same(t, replacement, sess)
	require.NotNil(t, capturedCtx, "newSession must have been called for the renewal")

	// The tool call returns and its ctx is cancelled - the renewed
	// session's own context must not observe it.
	cancelCaller()
	require.NoError(t, capturedCtx.Err(),
		"the renewed session's context must be detached from the caller's tool-call ctx")
}
