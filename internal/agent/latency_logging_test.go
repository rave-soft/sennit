package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// captureJSONLogs is captureLogs' JSON-encoded sibling: a test that only
// needs to know a log line fired can scrape text (see captureLogs), but a
// test asserting on a structured field's actual value (a duration) needs
// to decode it back out rather than pattern-match formatted text. See
// logCaptureMu (common_test.go) for why this serializes against every
// other log capture in the package rather than just swapping the global
// default outright.
func captureJSONLogs(t *testing.T) *syncLogBuffer {
	t.Helper()
	logCaptureMu.Lock()
	var b syncLogBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		logCaptureMu.Unlock()
	})
	return &b
}

// findLogLine returns the first captured line with the given message that
// belongs to sessionID.
//
// The session filter is not decoration: captureJSONLogs redirects the
// process-wide slog default, so with parallel tests in this package the
// buffer holds lines from whichever other tests happen to be running.
// Matching on the message alone made this read another test's line and
// assert against its counts.
func findLogLine(t *testing.T, buf *syncLogBuffer, msg, sessionID string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &decoded))
		if decoded["msg"] == msg && decoded["session"] == sessionID {
			return decoded
		}
	}
	return nil
}

// float64List asserts field decodes as a JSON array of numbers and returns
// it as []float64 (encoding/json always decodes a bare number as float64).
func float64List(t *testing.T, line map[string]any, field string) []float64 {
	t.Helper()
	raw, ok := line[field]
	require.True(t, ok, "log line missing field %q: %v", field, line)
	arr, ok := raw.([]any)
	require.True(t, ok, "field %q is not a list: %v", field, raw)
	out := make([]float64, len(arr))
	for i, v := range arr {
		n, ok := v.(float64)
		require.True(t, ok, "field %q entry %d is not a number: %v", field, i, v)
		out[i] = n
	}
	return out
}

// driveGatedSteeringAndCompletion runs a turn gated mid-step-1 (via a
// "hold" tool, the same technique TestPrepareStep_CompletionDeliveredBeforeSteering
// uses), lands completion and a steering follow-up in the window where the
// turn is busy, sleeps a real delay before letting step 2 run, and returns
// the JSON logs captured for the whole run. Both the completion's
// TerminalAt and the steering call's own enqueue stamp precede the sleep,
// so both measurements below span at least that real delay - not a
// trivially-zero same-instant reading.
func driveGatedSteeringAndCompletion(t *testing.T, completion TaskCompletion, steeringPrompt string, delay time.Duration) (*syncLogBuffer, string) {
	t.Helper()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	gate := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	hold := fantasy.NewAgentTool("hold", "hold", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		once.Do(func() { close(entered) })
		<-gate
		return fantasy.NewTextResponse("ok"), nil
	})

	model := &twoStepToolModel{}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{hold},
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main"})
		runDone <- runErr
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the hold tool never entered - step 1 never reached it")
	}

	completion.DelegationID = "task-1"
	completion.Kind = "task"
	completion.Status = "completed"
	completion.ChildSessionID = "child-session"
	completion.TerminalAt = time.Now()
	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: steeringPrompt})

	time.Sleep(delay)
	close(gate)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("run never completed step 2")
	}

	return logs, sess.ID
}

// TestPrepareStep_LogsSteeringAndCompletionLatency proves the two
// durations the plan asked for actually land on the log lines that mark
// those transitions: "Steering folded into turn" (submit - enqueueCall's
// stamp - to injection - the fold succeeding in prepareStep) and
// "Completion delivered" (a delegation reaching its terminal status -
// deliverCompletion's stamp - to the completion being delivered in a
// step). Both are driven through a real ~60ms delay between the stamp and
// the fold, so a passing assertion proves the value was actually measured
// rather than trivially zero.
func TestPrepareStep_LogsSteeringAndCompletionLatency(t *testing.T) {
	t.Parallel()

	const delay = 60 * time.Millisecond
	logs, sessionID := driveGatedSteeringAndCompletion(t, TaskCompletion{
		Name:       "task-name",
		Goal:       "background goal",
		ResultText: "background result",
	}, "steering follow-up", delay)

	// A little slack under the nominal delay: the sleep is a floor, not a
	// promise, but scheduling jitter should never make the measured wait
	// meaningfully less than what was actually slept.
	minMS := float64(delay.Milliseconds()) - 15

	steerLine := findLogLine(t, logs, "Steering folded into turn", sessionID)
	require.NotNil(t, steerLine, "expected a 'Steering folded into turn' log line")
	require.EqualValues(t, 1, steerLine["count"])
	steerWaits := float64List(t, steerLine, "waited_ms")
	require.Len(t, steerWaits, 1)
	require.GreaterOrEqual(t, steerWaits[0], 0.0, "a duration must never be negative")
	require.GreaterOrEqual(t, steerWaits[0], minMS, "must reflect the real delay, not be trivially zero")

	completionLine := findLogLine(t, logs, "Completion delivered", sessionID)
	require.NotNil(t, completionLine, "expected a 'Completion delivered' log line")
	require.EqualValues(t, 1, completionLine["count"])
	completionWaits := float64List(t, completionLine, "waited_ms")
	require.Len(t, completionWaits, 1)
	require.GreaterOrEqual(t, completionWaits[0], 0.0, "a duration must never be negative")
	require.GreaterOrEqual(t, completionWaits[0], minMS, "must reflect the real delay, not be trivially zero")
}

// TestPrepareStep_LatencyLogsCarryNoPromptOrResultText proves the privacy
// rule still holds on the two lines this step added a duration to: the
// completion's Goal/ResultText and the steering call's own prompt text
// must never reach a log line, only ids, statuses, counts, and the new
// durations.
func TestPrepareStep_LatencyLogsCarryNoPromptOrResultText(t *testing.T) {
	t.Parallel()

	logs, sessionID := driveGatedSteeringAndCompletion(t, TaskCompletion{
		Name:       "task-name",
		Goal:       "SECRET-GOAL-do-not-log-this",
		ResultText: "SECRET-RESULT-do-not-log-this",
	}, "SECRET-STEERING-PROMPT-do-not-log-this", 10*time.Millisecond)

	// Confirm both lines actually fired - a passing assertion below on an
	// empty/missing line would prove nothing.
	require.NotNil(t, findLogLine(t, logs, "Steering folded into turn", sessionID))
	require.NotNil(t, findLogLine(t, logs, "Completion delivered", sessionID))

	captured := logs.String()
	require.NotContains(t, captured, "SECRET-GOAL-do-not-log-this", "a completion's Goal must never reach a log line")
	require.NotContains(t, captured, "SECRET-RESULT-do-not-log-this", "a completion's ResultText must never reach a log line")
	require.NotContains(t, captured, "SECRET-STEERING-PROMPT-do-not-log-this", "a steering call's prompt text must never reach a log line")
}
