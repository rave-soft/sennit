package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// releaseGatedModel is a fantasy.LanguageModel whose Stream blocks until
// either release is closed or ctx ends, signalling started once the block
// is actually reached. It lets a test hold a title-generation call open
// for as long as it likes, deterministically, rather than racing a real
// timeout.
type releaseGatedModel struct {
	fakeStreamModel
	release chan struct{}
	started chan struct{}
}

func (m *releaseGatedModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		close(m.started)
		select {
		case <-ctx.Done():
		case <-m.release:
		}
	}, nil
}

func titleModel(model fantasy.LanguageModel) Model {
	return Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}}
}

// testTitleTimeout is the bound TestStartGenerateTitleTimesOutOnStalledProvider
// injects via sessionAgent.titleTimeout, in place of the 45s production
// titleGenerationTimeout. The assertion is that *some* bound is applied to
// the provider call, not that it is specifically 45s, so a bound measured
// in tens of milliseconds proves exactly as much while keeping this test
// (and the -race suite it runs in) fast.
const testTitleTimeout = 30 * time.Millisecond

// TestStartGenerateTitleTimesOutOnStalledProvider is the regression test
// for the leaked title goroutine: before this fix, run_turn.go fired
// generateTitle on a bare `go` with a context.WithoutCancel that carried no
// deadline of its own, so a provider that accepted the request and then
// said nothing left the goroutine (and its in-flight HTTP call) running
// for the lifetime of the process — exactly the condition
// internal/agent/provider_stall.go exists to handle for a turn's own
// stream, but title generation had no equivalent.
//
// Here the fake provider blocks forever (fakeStreamModel{block: true}
// only ever unblocks on ctx.Done()), and startGenerateTitle is given a
// background context with no deadline of its own — the same shape
// run_turn.go hands it. sessionAgent.titleTimeout is set to
// testTitleTimeout so the bound this test proves is applied at all, rather
// than the production window, actually fires. Closing the sessionAgent's
// readiness lifecycle waits for the goroutine startGenerateTitle launched;
// that wait must return well before "forever", bounded by the injected
// timeout, and no goroutine may be left behind once it does.
func TestStartGenerateTitleTimesOutOnStalledProvider(t *testing.T) {
	env := testEnv(t)

	model := &fakeStreamModel{script: streamScript{block: true}}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)
	lifecycle := &readinessLifecycle{}
	sa.lifecycle = lifecycle
	sa.titleTimeout = testTitleTimeout

	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	ignoreBaseline := goleak.IgnoreCurrent()

	start := time.Now()
	sa.startGenerateTitle(context.Background(), sess.ID, "hello world", titleModel(model), "")

	// Bounded well above testTitleTimeout so a genuine hang fails the
	// test instead of the assertion below silently passing on a
	// still-running goroutine.
	closeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, lifecycle.close(closeCtx), "the title goroutine must finish on its own, bounded by the injected timeout")
	require.Less(t, time.Since(start), 5*time.Second)

	// The deferred fallback in generateTitle saves the default name once
	// the bounded call gives up — proving the provider call itself
	// observed the deadline rather than the wrapper merely returning
	// early while the call kept running.
	got, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, DefaultSessionName, got.Title)

	deadline := time.Now().Add(2 * time.Second)
	var leakErr error
	for time.Now().Before(deadline) {
		leakErr = goleak.Find(ignoreBaseline)
		if leakErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, leakErr, "no goroutine (or its in-flight request) may survive a bounded title generation")
}

// TestCoordinatorCloseWaitsForTitleGeneration proves the second half of the
// fix: startGenerateTitle registers through readinessLifecycle.launch, so
// Coordinator.Close (turnDispatcher.Close in this package) actually knows
// about the goroutine and waits for it, instead of racing ahead unaware —
// the pre-fix `go a.generateTitle(...)` gave Close nothing to wait on.
//
// The model is release-gated rather than timeout-bound, so this test's
// timing is deterministic: Close must still be waiting while the call is
// held open, and must return promptly once it is released — mirroring
// TestCoordinatorCloseWaitsForBlockedFinalizerReadiness's idiom for
// buildAgent's own readiness goroutines.
func TestCoordinatorCloseWaitsForTitleGeneration(t *testing.T) {
	env := testEnv(t)

	model := &releaseGatedModel{release: make(chan struct{}), started: make(chan struct{})}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)
	lifecycle := &readinessLifecycle{}
	sa.lifecycle = lifecycle

	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	sa.startGenerateTitle(context.Background(), sess.ID, "hello world", titleModel(model), "")

	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		t.Fatal("title generation never reached the provider call")
	}

	closeCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, (&turnDispatcher{lifecycle: lifecycle}).Close(closeCtx), context.DeadlineExceeded,
		"Close must still be waiting on the in-flight title call")

	close(model.release)
	require.NoError(t, (&turnDispatcher{lifecycle: lifecycle}).Close(t.Context()))
}

// TestRunSkipsTitleGenerationForRetitledSubAgent proves the third half of
// the fix: a delegated sub-agent's first turn must not overwrite the title
// CreateSubAgentSession deliberately set (and must not spend an extra
// model call doing it), but a sub-agent session that genuinely has no
// title — an unusual construction path, but not one the turn should give
// up on — still gets one generated.
func TestRunSkipsTitleGenerationForRetitledSubAgent(t *testing.T) {
	env := testEnv(t)
	model := newRecordingModel("fake", "fake-model")

	sa := NewSessionAgent(SessionAgentOptions{
		Model:      titleModel(model),
		IsSubAgent: true,
		Sessions:   env.sessions,
		Messages:   env.messages,
	}).(*sessionAgent)

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	titled, err := env.sessions.CreateSubAgentSession(t.Context(), "child-titled", parent.ID, "Fetch Analysis", "")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: titled.ID, Prompt: "do the thing"})
	require.NoError(t, err)

	turns, titles := model.counts()
	require.Equal(t, 1, turns)
	require.Equal(t, 0, titles, "a sub-agent session with an existing title must not trigger title generation")

	untitled, err := env.sessions.CreateSubAgentSession(t.Context(), "child-untitled", parent.ID, "", "")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: untitled.ID, Prompt: "do another thing"})
	require.NoError(t, err)

	select {
	case <-model.titleGot:
	case <-time.After(2 * time.Second):
		t.Fatal("a sub-agent session with no title must still get one generated")
	}
}
