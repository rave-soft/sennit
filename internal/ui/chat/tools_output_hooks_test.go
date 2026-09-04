package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// hookMetadataJSON marshals a single-hook proto.HookMetadata into the
// wrapper shape toolOutputHookIndicator decodes ({"hook": {...}}).
func hookMetadataJSON(t *testing.T, hi proto.HookInfo) string {
	t.Helper()
	meta := struct {
		Hook proto.HookMetadata `json:"hook"`
	}{
		Hook: proto.HookMetadata{HookCount: 1, Decision: hi.Decision, Hooks: []proto.HookInfo{hi}},
	}
	data, err := json.Marshal(meta)
	require.NoError(t, err)
	return string(data)
}

// TestToolOutputHookIndicator_ReasonSanitizedToOneLine pins Audit 12
// finding 2: Reason is a blocking hook's raw stderr (see
// hooks/runner.go), which routinely spans multiple lines (a linter's
// output). oneLine sanitizes the Name column but Reason went straight to
// the renderer, so a multi-line Reason blew the single indicator line
// into several and could overflow the terminal horizontally.
func TestToolOutputHookIndicator_ReasonSanitizedToOneLine(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	metadata := hookMetadataJSON(t, proto.HookInfo{
		Name:     "format.sh",
		Decision: "deny",
		Reason:   "line one\nline two\nline three",
	})

	out := ansi.Strip(toolOutputHookIndicator(&sty, metadata, 200))

	require.Equal(t, 1, len(strings.Split(out, "\n")),
		"a multi-line Reason must collapse to a single rendered line")
	require.Contains(t, out, "line one¶line two¶line three",
		"newlines collapse to ¶ (kept, not dropped) the same way hook names already did")
}

// TestToolOutputHookIndicator_MatcherSanitizedToOneLine covers the Matcher
// column, the other one appendResultSummary's doc comment promised was
// sanitized but wasn't.
func TestToolOutputHookIndicator_MatcherSanitizedToOneLine(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	metadata := hookMetadataJSON(t, proto.HookInfo{
		Name:     "check.sh",
		Matcher:  "a\nb",
		Decision: "allow",
	})

	out := ansi.Strip(toolOutputHookIndicator(&sty, metadata, 200))
	require.Equal(t, 1, len(strings.Split(out, "\n")))
	require.Contains(t, out, "a¶b")
}

// TestToolOutputHookIndicator_TruncatesLongDetailToWidth pins the other
// half of finding 2: maxDetailWidth was computed but never applied — a
// long Reason was never truncated, so it could run the whole indicator
// line past the given width no matter how narrow width was.
func TestToolOutputHookIndicator_TruncatesLongDetailToWidth(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	longReason := strings.Repeat("x", 500)
	metadata := hookMetadataJSON(t, proto.HookInfo{
		Name:     "lint.sh",
		Decision: "deny",
		Reason:   longReason,
	})

	const width = 60
	out := toolOutputHookIndicator(&sty, metadata, width)
	for _, ln := range strings.Split(out, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(ln), width,
			"every rendered line must fit inside the given width, including the detail column")
	}
}

// TestToolOutputHookIndicator_HaltShownDistinctlyFromDeny pins the Halt
// half of finding 2: HookInfo.Halt is written by the agent
// (hooked_tool.go) and marks a hook that stopped the whole turn, not just
// this one call — a materially bigger consequence than an ordinary deny.
// The renderer used to read Decision only, so a halting hook looked
// exactly like any other denial.
func TestToolOutputHookIndicator_HaltShownDistinctlyFromDeny(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()

	deny := ansi.Strip(toolOutputHookIndicator(&sty, hookMetadataJSON(t, proto.HookInfo{
		Name: "check.sh", Decision: "deny", Reason: "nope",
	}), 200))
	halted := ansi.Strip(toolOutputHookIndicator(&sty, hookMetadataJSON(t, proto.HookInfo{
		Name: "check.sh", Decision: "deny", Halt: true, Reason: "nope",
	}), 200))

	require.Contains(t, deny, "Denied")
	require.NotContains(t, deny, "Halted")
	require.Contains(t, halted, "Halted")
	require.NotContains(t, halted, "Denied",
		"a halting hook must not read as an ordinary denial")
}
