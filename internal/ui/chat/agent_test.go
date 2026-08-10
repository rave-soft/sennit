package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// mkNestedToolCall builds a synthetic nested-tool item, mirroring what
// handleChildSessionMessage in internal/ui/model/ui.go creates from a
// live child-session pubsub event.
func mkNestedToolCall(t *testing.T, sty *styles.Styles, id, name, input string) ToolMessageItem {
	t.Helper()
	tc := message.ToolCall{ID: id, Name: name, Input: input, Finished: true}
	return NewToolMessageItem(sty, "child-msg", tc, nil, false)
}

// TestFormatElapsed covers the elapsed-time formatting used by the
// running status line: seconds-only, minutes+seconds, and hours+minutes.
func TestFormatElapsed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		d    time.Duration
		want string
	}{
		{9 * time.Second, "9s"},
		{45 * time.Second, "45s"},
		{4*time.Minute + 12*time.Second, "4m12s"},
		{time.Hour + 2*time.Minute, "1h02m"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, formatElapsed(tt.d))
	}
}

// TestLastToolSummary covers the "→ tool arg" fragment of the status
// line for the tool kinds a sub-agent commonly runs — this is the exact
// piece the bug report asked for: "→ grep "Provider" internal/config".
func TestLastToolSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tc   message.ToolCall
		want string
	}{
		{
			name: "grep with path",
			tc:   message.ToolCall{Name: "grep", Input: `{"pattern":"Provider","path":"internal/config"}`},
			want: `grep "Provider" internal/config`,
		},
		{
			name: "grep without path",
			tc:   message.ToolCall{Name: "grep", Input: `{"pattern":"Provider"}`},
			want: `grep "Provider"`,
		},
		{
			name: "view",
			tc:   message.ToolCall{Name: "view", Input: `{"file_path":"internal/foo.go"}`},
			want: "view internal/foo.go",
		},
		{
			name: "bash uses description over command",
			tc:   message.ToolCall{Name: "bash", Input: `{"description":"run tests","command":"go test ./..."}`},
			want: "bash run tests",
		},
		{
			name: "unrecognized tool falls back to bare name",
			tc:   message.ToolCall{Name: "some_mcp_tool", Input: `{}`},
			want: "some_mcp_tool",
		},
		{
			name: "unparsable input falls back to bare name",
			tc:   message.ToolCall{Name: "grep", Input: `not json`},
			want: "grep",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, lastToolSummary(tt.tc))
		})
	}
}

// TestRenderAgentStatusLine_Content builds the line end-to-end from
// synthetic nested-tool events and checks that elapsed time, step count,
// last-tool summary, and token count all show up in the expected order.
func TestRenderAgentStatusLine_Content(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	nested := []ToolMessageItem{
		mkNestedToolCall(t, &sty, "c1", "view", `{"file_path":"internal/foo.go"}`),
		mkNestedToolCall(t, &sty, "c2", "grep", `{"pattern":"Provider","path":"internal/config"}`),
	}

	start := time.Now().Add(-(4*time.Minute + 12*time.Second))
	line := renderAgentStatusLine(&sty, 200, start, nested, 1200, 300)
	plain := ansi.Strip(line)

	require.Contains(t, plain, "4m12s")
	require.Contains(t, plain, "step 2")
	require.Contains(t, plain, `→ grep "Provider" internal/config`)
	require.Contains(t, plain, "1.5k tok")

	// Order matters: elapsed, then step, then last tool, then tokens.
	elapsedIdx := strings.Index(plain, "4m12s")
	stepIdx := strings.Index(plain, "step 2")
	toolIdx := strings.Index(plain, "→ grep")
	tokIdx := strings.Index(plain, "1.5k tok")
	require.True(t, elapsedIdx < stepIdx && stepIdx < toolIdx && toolIdx < tokIdx,
		"status line fields must render in elapsed -> step -> last-tool -> tokens order, got: %s", plain)
}

// TestRenderAgentStatusLine_NoNestedTools covers the very first seconds
// of a delegation, before any child-session event has arrived. Elapsed
// time and step count must still render — this is what keeps the first
// stretch of a run from looking identical to a hang.
func TestRenderAgentStatusLine_NoNestedTools(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	line := renderAgentStatusLine(&sty, 200, time.Now(), nil, 0, 0)
	plain := ansi.Strip(line)

	require.Contains(t, plain, "step 0")
	require.NotContains(t, plain, "→")
	require.NotContains(t, plain, "tok")
}

// TestRenderAgentStatusLine_Truncation guards the width-bounded
// contract: the rendered line (stripped of ANSI styling) must never
// exceed the requested width, and a width of 0 must degrade to "" rather
// than panic or render garbage.
func TestRenderAgentStatusLine_Truncation(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	nested := []ToolMessageItem{
		mkNestedToolCall(t, &sty, "c1", "grep", `{"pattern":"a very very long pattern that should get cut off","path":"internal/config/somewhere/deep"}`),
	}

	for _, width := range []int{0, 1, 10, 20, 40} {
		line := renderAgentStatusLine(&sty, width, time.Now(), nested, 0, 0)
		require.LessOrEqualf(t, ansi.StringWidth(ansi.Strip(line)), width,
			"width %d: rendered line exceeds requested width: %q", width, line)
	}
}

// TestAgentToolMessageItem_PendingStatusLine is the render-path
// regression test for the reported bug: a still-running "agent"
// delegation with no nested tools yet must show more than a bare
// spinner — specifically, an elapsed-time status line — and once a
// child tool call arrives, that tool must appear in the rendered
// output.
func TestAgentToolMessageItem_PendingStatusLine(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false)
	item.startTime = time.Now().Add(-9 * time.Second)

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "9s", "pending agent tool with no nested tools yet must still show elapsed time")
	require.Contains(t, out, "step 0")

	item.AddNestedTool(mkNestedToolCall(t, &sty, "c1", "grep", `{"pattern":"Provider","path":"internal/config"}`))
	out = ansi.Strip(item.Render(120))
	require.Contains(t, out, `→ grep "Provider" internal/config`,
		"once a child tool call arrives it must show up in the rendered status line")
}

// TestAgentToolMessageItem_SetChildSessionTokensBumpsVersion covers the
// live token-count update path (handleChildSessionUpdate in
// internal/ui/model/ui.go): pushing a new token count must invalidate
// the cached render, and a repeated identical update must not bump
// (matching the no-op dedupe on every other setter in this file).
func TestAgentToolMessageItem_SetChildSessionTokensBumpsVersion(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false)

	requireBump(t, "SetChildSessionTokens[first update]", item, func() {
		item.SetChildSessionTokens(100, 50)
	})

	before := item.Version()
	item.SetChildSessionTokens(100, 50)
	require.Equal(t, before, item.Version(), "identical token counts must not bump the version")

	requireBump(t, "SetChildSessionTokens[changed]", item, func() {
		item.SetChildSessionTokens(200, 50)
	})
}

// TestAgenticFetchToolMessageItem_SetChildSessionTokensBumpsVersion is
// the agentic-fetch counterpart of the token-update bump test above.
func TestAgenticFetchToolMessageItem_SetChildSessionTokensBumpsVersion(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "fetch-parent", Name: "agentic_fetch", Input: `{}`, Finished: false}
	item := NewAgenticFetchToolMessageItem(&sty, parent, nil, false)

	requireBump(t, "SetChildSessionTokens[first update]", item, func() {
		item.SetChildSessionTokens(100, 50)
	})

	before := item.Version()
	item.SetChildSessionTokens(100, 50)
	require.Equal(t, before, item.Version(), "identical token counts must not bump the version")
}
