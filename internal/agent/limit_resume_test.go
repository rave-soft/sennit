package agent

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/stretchr/testify/require"
)

func limitErrorIn(d time.Duration) *ProviderLimitError {
	return &ProviderLimitError{
		Provider: "codex", ProviderName: "Codex", Plan: "plus",
		WindowMinutes: 300, UsedPercent: 100,
		ResetsAt: time.Now().Add(d),
		err:      &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests},
	}
}

// resumeTestAgent is the smallest agent the resume path needs: dispatcher
// state (for the busy check) and somewhere to record notifications.
func resumeTestAgent(notifier *recordingNotifier) *sessionAgent {
	return &sessionAgent{
		dispatcher:   newDispatcher(),
		limitResumes: newLimitResumes(),
		notify:       notifier,
	}
}

func TestLimitResumeWait(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	wait, ok := limitResumeWait(now.Add(3*time.Hour), now)
	require.True(t, ok)
	require.Equal(t, 3*time.Hour+resumeGrace, wait)

	wait, ok = limitResumeWait(now.Add(-time.Hour), now)
	require.True(t, ok, "a window already back is still resumed, after the grace")
	require.Equal(t, resumeGrace, wait)

	_, ok = limitResumeWait(now.Add(30*24*time.Hour), now)
	require.False(t, ok, "no real plan window resets a month out")
}

// TestScheduleLimitResume_ArmsAndReports is the feature in one test: a
// turn that ended on a usage limit leaves the session parked, and says so.
func TestScheduleLimitResume_ArmsAndReports(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	a := resumeTestAgent(notifier)
	a.scheduleLimitResume(t.Context(), SessionAgentCall{SessionID: "sess-1"}, limitErrorIn(2*time.Hour))

	require.True(t, a.limitResumes.pending("sess-1"))
	require.Equal(t, 1, notifier.count("sess-1", notify.TypeUsageLimitWaiting))

	a.limitResumes.disarm("sess-1")
	require.False(t, a.limitResumes.pending("sess-1"))
}

func TestScheduleLimitResume_OnlyForLimits(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	a := resumeTestAgent(notifier)

	a.scheduleLimitResume(t.Context(), SessionAgentCall{SessionID: "sess-1"}, errors.New("boom"))
	a.scheduleLimitResume(t.Context(), SessionAgentCall{SessionID: "sess-1"}, nil)
	require.False(t, a.limitResumes.pending("sess-1"), "only a usage limit parks a session")

	a.scheduleLimitResume(t.Context(), SessionAgentCall{SessionID: "sess-2", NonInteractive: true}, limitErrorIn(time.Hour))
	require.False(t, a.limitResumes.pending("sess-2"), "a one-shot run has nobody to come back to")

	sub := resumeTestAgent(notifier)
	sub.isSubAgent = true
	sub.scheduleLimitResume(t.Context(), SessionAgentCall{SessionID: "sess-3"}, limitErrorIn(time.Hour))
	require.False(t, sub.limitResumes.pending("sess-3"), "a delegation's parent is what gets resumed")
}

// TestScheduleLimitResume_ReplacesEarlierTimer keeps the one-timer-per-
// session rule honest: two limits in a row must not leave two resumes
// racing for the same idle slot.
func TestScheduleLimitResume_ReplacesEarlierTimer(t *testing.T) {
	t.Parallel()

	a := resumeTestAgent(&recordingNotifier{})
	a.scheduleLimitResume(t.Context(), SessionAgentCall{SessionID: "sess-1"}, limitErrorIn(time.Hour))
	first := a.limitResumes.timers["sess-1"]
	a.scheduleLimitResume(t.Context(), SessionAgentCall{SessionID: "sess-1"}, limitErrorIn(2*time.Hour))
	second := a.limitResumes.timers["sess-1"]
	require.NotSame(t, first, second)
	require.Len(t, a.limitResumes.timers, 1)
}

// TestResumeAfterLimit_RunsContinuation proves what the timer does when it
// fires: the session picks its own work back up through the continuation
// entry point, with no prompt of its own.
func TestResumeAfterLimit_RunsContinuation(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	a := resumeTestAgent(notifier)
	var mu sync.Mutex
	var ran []string
	a.continuationRunner = func(_ context.Context, sessionID string) error {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, sessionID)
		return nil
	}

	a.resumeAfterLimit(t.Context(), "sess-1", limitErrorIn(-time.Minute))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"sess-1"}, ran)
	require.Equal(t, 1, notifier.count("sess-1", notify.TypeUsageLimitResumed))
}

// TestResumeAfterLimit_SkipsBusySession: someone came back before the
// reset did, and their turn owns the session.
func TestResumeAfterLimit_SkipsBusySession(t *testing.T) {
	t.Parallel()

	a := resumeTestAgent(&recordingNotifier{})
	var ran int
	a.continuationRunner = func(context.Context, string) error {
		ran++
		return nil
	}
	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.setActiveForTest("sess-1", &activeCancel{cancel: cancel})

	a.resumeAfterLimit(t.Context(), "sess-1", limitErrorIn(-time.Minute))
	require.Zero(t, ran)
}

// TestResumeAfterLimit_StopsOnShutdown: the coordinator went away while
// the session was parked.
func TestResumeAfterLimit_StopsOnShutdown(t *testing.T) {
	t.Parallel()

	a := resumeTestAgent(&recordingNotifier{})
	var ran int
	a.continuationRunner = func(context.Context, string) error {
		ran++
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	a.resumeAfterLimit(ctx, "sess-1", limitErrorIn(-time.Minute))
	require.Zero(t, ran)
}
