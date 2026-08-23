package chat

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/stretchr/testify/require"
)

// TestFormatParametersForCopy_BashPreservesHeredocNewlines pins that
// copying a bash tool call keeps the command's newlines intact. Flattening
// them to spaces (as the single-line shell header display does) breaks a
// heredoc — "cat <<EOF\nbody\nEOF" pasted back as one line is not the same
// command.
func TestFormatParametersForCopy_BashPreservesHeredocNewlines(t *testing.T) {
	t.Parallel()

	cmd := "cat <<EOF\nhello world\nEOF"
	input, err := json.Marshal(tools.BashParams{Command: cmd})
	require.NoError(t, err)

	item := &baseToolMessageItem{
		toolCall: message.ToolCall{ID: "tc-bash", Name: tools.BashToolName, Input: string(input), Finished: true},
	}

	out := item.formatParametersForCopy()

	require.Contains(t, out, cmd, "heredoc command must survive the copy with its newlines intact")
}

// TestFormatParametersForCopy_GenericParamsSortedDeterministically pins
// that the fallback (unrecognized-tool) parameter formatter sorts keys
// before rendering. Go randomizes map iteration order, so without sorting
// the copied text would shuffle between renders of the same tool call.
func TestFormatParametersForCopy_GenericParamsSortedDeterministically(t *testing.T) {
	t.Parallel()

	input := `{"zeta":"1","alpha":"2","mike":"3","bravo":"4","yankee":"5"}`
	item := &baseToolMessageItem{
		toolCall: message.ToolCall{ID: "tc-generic", Name: "some_custom_tool", Input: input, Finished: true},
	}

	first := item.formatParametersForCopy()
	for range 20 {
		require.Equal(t, first, item.formatParametersForCopy(),
			"repeated copies of the same params must render in the same order")
	}

	require.Equal(t, "**Alpha:** 2\n**Bravo:** 4\n**Mike:** 3\n**Yankee:** 5\n**Zeta:** 1", first)
}
