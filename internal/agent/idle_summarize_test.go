package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// idleSweepFixture is one dispatcher wired for the idle summarize sweep and
// nothing else: a real config store loaded from cfgJSON (so the options
// block is parsed the way a user's file is), a real session store, a real
// session agent for the busy check, and a recording stand-in for the
// summarize itself.
type idleSweepFixture struct {
	dispatcher *turnDispatcher
	agent      *sessionAgent
	sessions   session.Service
	summarized []string
}

// newIdleSweepFixture builds the fixture around a session carrying
// promptTokens. It uses t.Setenv (through writeGlobalConfig), so callers
// must not be parallel tests.
func newIdleSweepFixture(t *testing.T, cfgJSON string, promptTokens int64) (*idleSweepFixture, session.Session) {
	t.Helper()
	writeGlobalConfig(t, cfgJSON)
	env := testEnv(t)
	store, err := configruntime.Load(env.workingDir, "", false)
	require.NoError(t, err)

	sess, err := env.sessions.Create(t.Context(), "idle session")
	require.NoError(t, err)
	sess.PromptTokens = promptTokens
	sess, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	agent := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: &raceInjectModel{text: "summary"}, CatalogCfg: catwalk.Model{ContextWindow: 200000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	f := &idleSweepFixture{agent: agent, sessions: env.sessions}
	f.dispatcher = &turnDispatcher{
		cfg:          store,
		sessions:     env.sessions,
		agentPort:    &coordinatorAgentPort{agent: agent},
		lifecycle:    &readinessLifecycle{},
		lastActivity: csync.NewMap[string, time.Time](),
		summarizeIdle: func(_ context.Context, sessionID string) error {
			f.summarized = append(f.summarized, sessionID)
			return nil
		},
	}
	return f, sess
}

// idleConfig is a global config file that turns the idle pass on with the
// given thresholds.
const idleConfig = `{"options":{"auto_summarize_idle":{"enabled":true,"context_tokens":60000,"after":"4m"}}}`

// TestIdleSweep_SummarizesALargeSessionOnceItGoesQuiet is the feature
// itself: a session over the context threshold that has seen no work for
// longer than the configured window is summarized where it stands, without
// waiting for the next turn to walk into the context window.
func TestIdleSweep_SummarizesALargeSessionOnceItGoesQuiet(t *testing.T) {
	f, sess := newIdleSweepFixture(t, idleConfig, 80_000)
	f.dispatcher.markActivity(sess.ID)
	marked, ok := f.dispatcher.lastActivity.Get(sess.ID)
	require.True(t, ok)

	// Still inside the idle window: the person may just be reading.
	f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(3*time.Minute))
	require.Empty(t, f.summarized, "a session idle for less than the window must be left alone")

	f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(5*time.Minute))
	require.Equal(t, []string{sess.ID}, f.summarized)

	// And it restarted the idle clock on the way, so the session has to
	// go quiet for another whole window before it is picked up again — a
	// summarize that failed, or one still finishing, costs one attempt
	// per window rather than one every sweep.
	after, ok := f.dispatcher.lastActivity.Get(sess.ID)
	require.True(t, ok)
	require.True(t, after.After(marked), "the sweep must restart the session's idle clock")
}

// TestIdleSweep_LeavesSmallSessionsAlone: idleness alone is not a reason to
// summarize. A short conversation has no room to reclaim, and compressing
// it would spend a request to throw detail away.
func TestIdleSweep_LeavesSmallSessionsAlone(t *testing.T) {
	f, sess := newIdleSweepFixture(t, idleConfig, 59_999)
	f.dispatcher.markActivity(sess.ID)

	f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(time.Hour))
	require.Empty(t, f.summarized, "a session under the context threshold must never be summarized on idleness")
}

// TestIdleSweep_RespectsTheSwitches: the pass has its own on/off switch,
// and the global disable_auto_summarize still wins over it — that switch
// means "do not summarize this session behind my back", and an idle pass is
// exactly that.
func TestIdleSweep_RespectsTheSwitches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfgJSON string
	}{
		{"the idle pass turned off", `{"options":{"auto_summarize_idle":{"enabled":false}}}`},
		{"auto-summarize turned off entirely", `{"options":{"disable_auto_summarize":true,"auto_summarize_idle":{"enabled":true}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := newIdleSweepFixture(t, tc.cfgJSON, 500_000)
			f.dispatcher.markActivity(sess.ID)
			f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(time.Hour))
			require.Empty(t, f.summarized)
		})
	}
}

// TestIdleSweep_SkipsABusySession: a session with a run in flight is not
// idle, however long ago its last turn started. Summarizing under a running
// turn would rewrite the context that turn is working in.
func TestIdleSweep_SkipsABusySession(t *testing.T) {
	f, sess := newIdleSweepFixture(t, idleConfig, 80_000)
	f.dispatcher.markActivity(sess.ID)

	ac := &activeCancel{cancel: func() {}}
	f.agent.setActiveForTest(sess.ID, ac)
	f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(time.Hour))
	require.Empty(t, f.summarized, "a busy session must not be summarized out from under its own turn")

	f.agent.clearActiveIfMatch(sess.ID, ac)
	f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(time.Hour))
	require.Equal(t, []string{sess.ID}, f.summarized, "and it is picked up once the turn is done")
}

// TestIdleSweep_ForgetsADeletedSession: a session that no longer exists must
// not be retried on every tick for the rest of the process's life.
func TestIdleSweep_ForgetsADeletedSession(t *testing.T) {
	f, sess := newIdleSweepFixture(t, idleConfig, 80_000)
	f.dispatcher.markActivity(sess.ID)
	require.NoError(t, f.sessions.Delete(t.Context(), sess.ID))

	f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(time.Hour))
	require.Empty(t, f.summarized)
	_, watched := f.dispatcher.lastActivity.Get(sess.ID)
	require.False(t, watched, "a session that is gone must be dropped from the idle watch")
}

// TestIdleSweep_ReadsTheConfiguredThresholds: the numbers are the user's,
// not the defaults'. A file that says 10k tokens and 30 seconds gets 10k
// tokens and 30 seconds.
func TestIdleSweep_ReadsTheConfiguredThresholds(t *testing.T) {
	f, sess := newIdleSweepFixture(t,
		`{"options":{"auto_summarize_idle":{"context_tokens":10000,"after":"30s"}}}`, 10_001)
	f.dispatcher.markActivity(sess.ID)

	f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(20*time.Second))
	require.Empty(t, f.summarized)

	f.dispatcher.sweepIdleSessions(t.Context(), time.Now().Add(45*time.Second))
	require.Equal(t, []string{sess.ID}, f.summarized,
		"a session over the configured size, idle past the configured window, must be summarized")
}

// TestAutoSummarizeIdleOptions_Defaults pins what an absent or half-filled
// config block means. Every accessor is nil-safe because the sweep reads
// them straight off Options, where the block is usually missing entirely.
func TestAutoSummarizeIdleOptions_Defaults(t *testing.T) {
	t.Parallel()

	var absent *config.AutoSummarizeIdleOptions
	require.True(t, absent.IsEnabled(), "an unconfigured idle pass is on")
	require.Equal(t, int64(config.DefaultAutoSummarizeIdleContextTokens), absent.EffectiveContextTokens())
	require.Equal(t, config.DefaultAutoSummarizeIdleAfter, absent.EffectiveAfter())

	off := false
	require.False(t, (&config.AutoSummarizeIdleOptions{Enabled: &off}).IsEnabled())

	// A value that cannot be honored falls back to the default rather
	// than to "summarize immediately", which is what zero and an
	// unparseable duration would otherwise mean.
	bad := &config.AutoSummarizeIdleOptions{ContextTokens: -1, After: "4 minutes"}
	require.Equal(t, int64(config.DefaultAutoSummarizeIdleContextTokens), bad.EffectiveContextTokens())
	require.Equal(t, config.DefaultAutoSummarizeIdleAfter, bad.EffectiveAfter())

	set := &config.AutoSummarizeIdleOptions{ContextTokens: 1234, After: "90s"}
	require.Equal(t, int64(1234), set.EffectiveContextTokens())
	require.Equal(t, 90*time.Second, set.EffectiveAfter())
}
