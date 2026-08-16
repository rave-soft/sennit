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
	client := mcp.NewClient(&mcp.Implementation{Name: "braid-test"}, nil)
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
	client := mcp.NewClient(&mcp.Implementation{Name: "braid-test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	return &ClientSession{ClientSession: clientSession, cancel: cancel}
}

// TestUpdateState_ErrorClosesSessionAndClearsTools pins the primary fix: a
// StateError transition must (1) remove the session from the map, (2) actually
// close it so its child process/pipes are released, and (3) clear its tools
// from the registry. Before the fix defaultRegistry.updateState only did a bare
// defaultRegistry.sessions.Del(name): the session was leaked and its tools lingered, so
// braid_info kept reading "connected, N tools" while the LLM's tool list and
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

// TestGetOrRenewClient_SerializesConcurrentRenewals is the concurrency
// regression the production renew path needs: when several tool calls observe
// the same dead session at once they must not each rebuild it. Without
// serialization, concurrent renewals close a session another goroutine just
// registered or overwrite and leak a live replacement. With the per-server
// lock only the first arrival rebuilds; the rest re-check and reuse the
// healthy session, so exactly one new session is created.
func TestGetOrRenewClient_StalePingCannotReplaceNewSession(t *testing.T) {
	const name = "test-stale-ping"
	r := NewRegistry()
	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
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
	fresh, _ := liveSession(t, "fresh")
	freshOwner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, fresh)
	r.sessionOwners[name] = freshOwner
	r.publishMu.Unlock()
	close(releasePing)
	require.ErrorIs(t, <-result, context.Canceled)
	published, ok := r.sessions.Get(name)
	require.True(t, ok)
	require.Same(t, fresh, published)
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

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

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

// TestRegisterSessionTools_PopulatesRegistry pins that defaultRegistry.registerSessionTools —
// the single seam through which a (re)connected session's tools enter the
// registry — lists a live session's tools and writes them to defaultRegistry.allTools.
func TestRegisterSessionTools_PopulatesRegistry(t *testing.T) {
	const name = "test-register-tools"
	t.Cleanup(func() { defaultRegistry.allTools.Del(name) })

	sess, _ := liveSession(t, "send_message")
	t.Cleanup(func() { _ = sess.Close() })

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	count, err := defaultRegistry.registerSessionTools(context.Background(), cfg, name, sess)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	got, ok := defaultRegistry.allTools.Get(name)
	require.True(t, ok, "a live session's tools must be registered")
	require.Len(t, got, 1)
	require.Equal(t, "send_message", got[0].Name)
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

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	// 1. Initial connect registers the tool (mirrors defaultRegistry.initClient).
	sess1, _ := liveSession(t, "send_message")
	defaultRegistry.sessions.Set(name, sess1)
	_, err := defaultRegistry.registerSessionTools(context.Background(), cfg, name, sess1)
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
	count, err := defaultRegistry.registerSessionTools(context.Background(), cfg, name, sess2)
	require.NoError(t, err)
	defaultRegistry.sessions.Set(name, sess2)
	require.Equal(t, 1, count)

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

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

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
