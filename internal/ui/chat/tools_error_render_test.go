package chat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestToolErrorExpectedRefusalIsWarn asserts that the read-first and
// stale-file refusals — protocol nudges the agent recovers from on its own —
// render as WARN, not as the destructive-red ERROR tag.
func TestToolErrorExpectedRefusalIsWarn(t *testing.T) {
	sty := styles.SennitDark()
	for _, content := range []string{
		"cannot edit /a/b/c.go: it has not been read in this session.\n\nRead /a/b/c.go, then retry this edit.",
		"cannot edit /a/b/c.go: it changed on disk after you read it (modified X, last read Y).",
		"cannot edit /a/b/c.go at lines 3-9: that part of the file has not been read in this session.",
	} {
		out := ansi.Strip(toolErrorContent(&sty, &message.ToolResult{Content: content}, 80))
		require.Contains(t, out, "WARN", content)
		require.NotContains(t, out, "ERROR", content)
	}
}

// TestToolErrorGenuineFailureStaysError keeps the red tag for errors that are
// actually failures.
func TestToolErrorGenuineFailureStaysError(t *testing.T) {
	sty := styles.SennitDark()
	out := ansi.Strip(toolErrorContent(&sty, &message.ToolResult{Content: "old_string not found in file"}, 80))
	require.Contains(t, out, "ERROR")
}

// TestToolErrorKeepsReasonVisible asserts that a long absolute path inside an
// error is home-shortened and elided from the head — the way tool headers
// show paths — so the reason after it survives instead of the line being cut
// off mid-path.
func TestToolErrorKeepsReasonVisible(t *testing.T) {
	sty := styles.SennitDark()
	path := filepath.Join(home.Dir(), "Projects", "some", "deeply", "nested", "place", "tools_render.go")
	content := "cannot edit " + path + ": it has not been read in this session. Read " + path + ", then retry this edit."

	out := ansi.Strip(toolErrorContent(&sty, &message.ToolResult{Content: content}, 80))
	require.Contains(t, out, "tools_render.go", "the file name identifies the file")
	require.Contains(t, out, "has not been read", "the reason must survive truncation")
	require.NotContains(t, out, home.Dir(), "the home prefix is shortened away")
	require.LessOrEqual(t, ansi.StringWidth(out), 80)
	require.False(t, strings.Contains(out, "\n"), "the error stays one line")
}
