package message

import "fmt"

// WorkingPhase names what an assistant message is doing while it is still
// in flight. It exists so every "the agent is busy" affordance — the
// chat's working animation, a pending tool's spinner, the non-interactive
// spinner in `sennit run` — answers "doing what?" from the same reading of
// the same message, instead of each one inventing its own wording or, as
// before, showing a bare animation with no wording at all.
//
// The phases are deliberately coarse: they are derived from state the
// message already carries, so they cost nothing to compute on every frame
// and cannot go stale relative to what is rendered beside them.
type WorkingPhase int

const (
	// PhaseIdle means nothing is in flight: the message has finished, or
	// it is not an assistant message at all. Its label is empty.
	PhaseIdle WorkingPhase = iota

	// PhaseThinking means reasoning content is streaming and no answer
	// text has started yet.
	PhaseThinking

	// PhaseSummarizing means the message is the compaction summary the
	// agent writes when a session is condensed, not a reply.
	PhaseSummarizing

	// PhaseCallingTool means the provider is streaming a tool call's
	// arguments: the call is known, its input is still arriving.
	PhaseCallingTool

	// PhaseWorking is the fallback for an unfinished assistant message
	// none of the above describes — most often the gap between the
	// request going out and the first token coming back.
	PhaseWorking
)

// Working is a phase plus whatever detail that phase carries.
type Working struct {
	Phase WorkingPhase

	// Tool is the name of the tool whose arguments are streaming. Set
	// only for [PhaseCallingTool].
	Tool string
}

// Label is the human-readable wording for the phase, suitable as a
// spinner label. It is empty for [PhaseIdle], which callers can treat as
// "say nothing" or substitute their own default for.
func (w Working) Label() string {
	switch w.Phase {
	case PhaseThinking:
		return "Thinking"
	case PhaseSummarizing:
		return "Summarizing"
	case PhaseCallingTool:
		if w.Tool == "" {
			return "Calling tool"
		}
		return fmt.Sprintf("Calling %s", w.Tool)
	case PhaseWorking:
		return "Working"
	default:
		return ""
	}
}

// Working reports what the message is currently doing. A message that has
// finished, or that was never an assistant message, is [PhaseIdle].
//
// The order of the checks is the order of specificity, and it preserves
// what the chat's working animation did before this existed: thinking
// wins over the summary flag, because a summary message that is still
// reasoning is better described by what it is doing than by what it will
// become.
func (m *Message) Working() Working {
	if m.Role != Assistant || m.IsFinished() {
		return Working{}
	}
	switch {
	case m.IsThinking():
		return Working{Phase: PhaseThinking}
	case m.IsSummaryMessage:
		return Working{Phase: PhaseSummarizing}
	}
	// An unfinished tool call is one whose arguments the provider is
	// still streaming; a finished one is already executing, and the tool
	// itself reports that, not the message.
	for _, tc := range m.ToolCalls() {
		if !tc.Finished {
			return Working{Phase: PhaseCallingTool, Tool: tc.Name}
		}
	}
	return Working{Phase: PhaseWorking}
}
