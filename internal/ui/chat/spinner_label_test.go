package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestAssistantSpinnerNamesThePhase: the working animation used to carry a
// label only while the model was thinking or summarizing. The most common
// case - request sent, nothing back yet - rendered as a bare band of
// glyphs and an elapsed timer, which says how long something unnamed has
// been taking. Every phase the animation is up for now has a word.
func TestAssistantSpinnerNamesThePhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		parts []message.ContentPart
		want  string
	}{
		{name: "waiting on the first token", want: "Working"},
		{
			name:  "reasoning",
			parts: []message.ContentPart{message.ReasoningContent{Thinking: "hmm"}},
			want:  "Thinking",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sty := styles.SennitDark()
			msg := &message.Message{ID: "a1", Role: message.Assistant, Parts: tt.parts}
			item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
			require.True(t, item.isSpinning(), "the fixture must still be working")

			require.Contains(t, ansi.Strip(item.renderSpinning()), tt.want)
		})
	}
}

// TestAssistantSpinnerLabelFollowsThePhase: the label is pushed into the
// animation only when the wording changes (SetLabel re-renders it rune by
// rune), so it must still follow a message that moves from one phase to
// the next mid-stream.
func TestAssistantSpinnerLabelFollowsThePhase(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msg := &message.Message{ID: "a1", Role: message.Assistant}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
	require.Contains(t, ansi.Strip(item.renderSpinning()), "Working")

	msg.Parts = []message.ContentPart{message.ReasoningContent{Thinking: "hmm"}}
	render := ansi.Strip(item.renderSpinning())
	require.Contains(t, render, "Thinking")
	require.NotContains(t, render, "Working")
}

// TestPendingToolSpinnerSaysArgumentsAreStreaming: a call whose arguments
// are still arriving is not yet executing. Saying so is what separates
// that wait from the tool's own runtime, which the block reports
// separately once the call lands.
func TestPendingToolSpinnerSaysArgumentsAreStreaming(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	call := message.ToolCall{ID: "tc-1", Name: "write", Input: `{"pa`}
	item := newBaseToolMessageItem(&sty, call, nil, &GenericToolRenderContext{}, false)
	require.True(t, item.isSpinning(), "the fixture must still be streaming arguments")

	require.Contains(t, ansi.Strip(item.RawRender(80)), "Preparing")
}
