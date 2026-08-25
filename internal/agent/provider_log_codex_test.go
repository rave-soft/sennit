package agent

import (
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/oauth/codex"
)

// retireCodexUsage backdates the current snapshot far enough that no later
// request can mistake it for its own. codex.LatestUsage is process-global
// (one machine, one signed-in account), so a snapshot left behind would
// outlive the test that wrote it.
func retireCodexUsage(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		codex.RecordUsage(codex.Usage{CapturedAt: time.Unix(0, 0)})
	})
}

// runInstrumentedStream drives one finished request through the instrumented
// model and returns its "Provider request finished" line. duringRequest runs
// after the model is wrapped but before the stream is consumed, which is
// where a real Codex response lands its snapshot: the transport reads the
// headers on the way back, between the attempt starting and the finished
// line being written.
func runInstrumentedStream(t *testing.T, buf *syncLogBuffer, sessionID, providerID string, duringRequest func()) map[string]any {
	t.Helper()
	corr := providerCorrelation{sessionID: sessionID, runID: "run-1", turnID: "turn-1", step: 0, attempt: 1, reason: reasonTurn}
	inner := &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 2}},
	}}}
	model := newInstrumentedModel(inner, corr, providerID)
	if duringRequest != nil {
		duringRequest()
	}

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	consumeStream(stream)

	lines := allProviderLogLines(t, buf, "Provider request finished", sessionID)
	require.Len(t, lines, 1)
	return lines[0]
}

// TestProviderLog_CodexPlanUsage covers the point of logging the allowance
// at all: a Codex request records what it left of the plan, so "why is the
// allowance gone" is answerable from the log instead of guessed at from
// token counts.
func TestProviderLog_CodexPlanUsage(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	retireCodexUsage(t)
	resets := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)

	line := runInstrumentedStream(t, logs, "sess-codex", codex.ProviderID, func() {
		codex.RecordUsage(codex.Usage{
			Plan:       "pro",
			Primary:    codex.UsageWindow{UsedPercent: 73, WindowMinutes: 10080, ResetsAt: resets},
			Secondary:  codex.UsageWindow{UsedPercent: 12, WindowMinutes: 60},
			CapturedAt: time.Now(),
		})
	})

	require.Equal(t, "pro", line["codex_plan"])
	require.EqualValues(t, 73, line["codex_primary_used_percent"])
	require.EqualValues(t, 10080, line["codex_primary_window_minutes"])
	require.Equal(t, resets.Format(time.RFC3339), line["codex_primary_resets_at"])
	require.EqualValues(t, 12, line["codex_secondary_used_percent"])
	require.EqualValues(t, 60, line["codex_secondary_window_minutes"])
	require.NotContains(t, line, "codex_secondary_resets_at",
		"the backend said nothing about this window's reset; inventing one would be worse than omitting it")
}

// TestProviderLog_CodexPlanUsageOmitted covers every case where a figure
// would be wrong rather than merely missing. The snapshot is global, so
// whether it belongs to *this* request is the whole question.
func TestProviderLog_CodexPlanUsageOmitted(t *testing.T) {
	t.Parallel()

	record := func(capturedAt time.Time) func() {
		return func() {
			codex.RecordUsage(codex.Usage{
				Plan:       "pro",
				Primary:    codex.UsageWindow{UsedPercent: 73, WindowMinutes: 10080},
				CapturedAt: capturedAt,
			})
		}
	}

	t.Run("another provider's request", func(t *testing.T) {
		t.Parallel()
		logs := captureJSONLogs(t)
		retireCodexUsage(t)
		// A local model streaming while Codex requests fly alongside it
		// would otherwise report an allowance it never spent.
		line := runInstrumentedStream(t, logs, "sess-local", "qwen36-local", record(time.Now()))
		require.NotContains(t, line, "codex_plan")
		require.NotContains(t, line, "codex_primary_used_percent")
	})

	t.Run("a snapshot older than the request", func(t *testing.T) {
		t.Parallel()
		logs := captureJSONLogs(t)
		retireCodexUsage(t)
		// It is some earlier request's reading; printing it here would
		// read as this request's price.
		line := runInstrumentedStream(t, logs, "sess-stale", codex.ProviderID, record(time.Now().Add(-time.Hour)))
		require.NotContains(t, line, "codex_plan")
	})

	t.Run("no snapshot at all", func(t *testing.T) {
		t.Parallel()
		logs := captureJSONLogs(t)
		retireCodexUsage(t)
		line := runInstrumentedStream(t, logs, "sess-none", codex.ProviderID, nil)
		require.NotContains(t, line, "codex_plan")
	})
}

// TestCodexWindowLogFields_UnknownWindowOmitted pins the distinction
// providerUsageLogFields already draws for tokens: a plan with no secondary
// window is not a plan with an empty one, and logging zeros would read as
// "an hourly limit, none of it used".
func TestCodexWindowLogFields_UnknownWindowOmitted(t *testing.T) {
	t.Parallel()

	require.Nil(t, codexWindowLogFields("codex_secondary", codex.UsageWindow{}))
	require.Nil(t, codexWindowLogFields("codex_secondary", codex.UsageWindow{UsedPercent: 40}),
		"a percentage without a window length is not a window this account has")
	require.NotEmpty(t, codexWindowLogFields("codex_primary", codex.UsageWindow{WindowMinutes: 60}))
}
