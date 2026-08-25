package message

import "testing"

func TestWorkingPhases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		msg   Message
		want  WorkingPhase
		label string
	}{
		{
			name:  "user message is idle",
			msg:   Message{Role: User, Parts: []ContentPart{TextContent{Text: "hi"}}},
			want:  PhaseIdle,
			label: "",
		},
		{
			name:  "finished assistant message is idle",
			msg:   Message{Role: Assistant, Parts: []ContentPart{TextContent{Text: "done"}, Finish{Reason: FinishReasonEndTurn}}},
			want:  PhaseIdle,
			label: "",
		},
		{
			name:  "request sent, nothing back yet",
			msg:   Message{Role: Assistant},
			want:  PhaseWorking,
			label: "Working",
		},
		{
			name:  "reasoning streaming",
			msg:   Message{Role: Assistant, Parts: []ContentPart{ReasoningContent{Thinking: "hmm"}}},
			want:  PhaseThinking,
			label: "Thinking",
		},
		{
			name:  "compaction summary",
			msg:   Message{Role: Assistant, IsSummaryMessage: true},
			want:  PhaseSummarizing,
			label: "Summarizing",
		},
		{
			// Thinking wins over the summary flag: what it is doing beats
			// what it will become.
			name:  "summary that is still reasoning",
			msg:   Message{Role: Assistant, IsSummaryMessage: true, Parts: []ContentPart{ReasoningContent{Thinking: "hmm"}}},
			want:  PhaseThinking,
			label: "Thinking",
		},
		{
			name:  "tool call arguments streaming",
			msg:   Message{Role: Assistant, Parts: []ContentPart{ToolCall{ID: "1", Name: "view", Finished: false}}},
			want:  PhaseCallingTool,
			label: "Calling view",
		},
		{
			// A finished call is executing; the tool reports that, not
			// the message.
			name:  "tool call complete",
			msg:   Message{Role: Assistant, Parts: []ContentPart{ToolCall{ID: "1", Name: "view", Finished: true}}},
			want:  PhaseWorking,
			label: "Working",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.msg.Working()
			if got.Phase != tt.want {
				t.Errorf("Phase = %v, want %v", got.Phase, tt.want)
			}
			if label := got.Label(); label != tt.label {
				t.Errorf("Label() = %q, want %q", label, tt.label)
			}
		})
	}
}

// A tool call with no name still gets a word rather than a dangling
// "Calling ".
func TestWorkingLabelUnnamedTool(t *testing.T) {
	t.Parallel()
	w := Working{Phase: PhaseCallingTool}
	if got := w.Label(); got != "Calling tool" {
		t.Errorf("Label() = %q, want %q", got, "Calling tool")
	}
}
