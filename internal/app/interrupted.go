package app

import (
	"context"
	"log/slog"

	"github.com/rave-soft/sennit/internal/message"
)

// interruptedToolResult is what a tool call that never came back gets
// recorded as. It is an error result on purpose: the call did not
// succeed, and the next turn to read this history has to see that rather
// than an empty success.
const interruptedToolResult = "Error: sennit exited before this tool call finished"

// interruptedFinishMessage is the Finish recorded on the assistant message
// whose turn was cut short.
const interruptedFinishMessage = "Interrupted: sennit exited before this turn finished"

// finalizeInterruptedTurns closes out the turns a previous process was
// killed in the middle of, for the sessions belonging to projectPath.
//
// Every path that ends a turn — normal completion, provider error, user
// cancel — writes a Finish on the assistant message and a result for each
// tool call, and every one of those paths runs inside the process that
// owns the turn (see internal/agent's runTurn cleanup and
// persistCanceledTurn). A kill -9, a crash, or a closed laptop runs none
// of them, so what is left behind is an assistant message with no Finish
// and tool calls with no results.
//
// Nothing repaired that. The record stayed that way through every
// restart, and the UI reads exactly this shape as "still running": a tool
// call with no result, not finished, not cancelled, is pending (see
// chat.ToolRenderOpts.IsPending), so it span forever. A sub-agent
// interrupted once left a spinner in its parent's transcript for the life
// of that session.
//
// It runs at bootstrap, before anything of this project's is dispatched,
// which is what makes "unfinished" safe to read as "abandoned". A live
// turn cannot be caught here: within the process, nothing has started
// yet; across processes, the workspace lock this bootstrap holds is the
// same one every other sennit instance on this repository must take, so
// there is no second process running turns against these sessions.
//
// Errors are logged rather than returned to the caller's caller: this
// repairs the record of work already over, and refusing to start because
// it could not be tidied would trade a stale spinner for no session at
// all.
func finalizeInterruptedTurns(ctx context.Context, projectPath string, messages message.Service) error {
	unfinished, err := messages.ListUnfinishedAssistantMessages(ctx, projectPath)
	if err != nil {
		return err
	}
	if len(unfinished) == 0 {
		return nil
	}

	// Tool results live in their own messages (Role: tool), so answering
	// a call means reading the rest of its session. Sessions are read
	// once and shared across every unfinished message they contain.
	answered := make(map[string]map[string]struct{})

	for _, msg := range unfinished {
		calls := msg.ToolCalls()
		seen, ok := answered[msg.SessionID]
		if !ok {
			seen, err = answeredToolCalls(ctx, messages, msg.SessionID)
			if err != nil {
				slog.Error("Failed to read a session while closing out an interrupted turn",
					"component", "app", "session_id", msg.SessionID, "error", err)
				continue
			}
			answered[msg.SessionID] = seen
		}

		for _, tc := range calls {
			if _, done := seen[tc.ID]; done {
				continue
			}
			if _, createErr := messages.Create(ctx, msg.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{message.ToolResult{
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    interruptedToolResult,
					IsError:    true,
				}},
			}); createErr != nil {
				slog.Error("Failed to record an interrupted tool call",
					"component", "app", "session_id", msg.SessionID, "tool_call_id", tc.ID, "error", createErr)
				continue
			}
			// Recorded, so a sibling unfinished message in the same
			// session does not answer it a second time.
			seen[tc.ID] = struct{}{}
		}

		// The Finish is what takes the message out of this query's reach,
		// so it is written last: an error above leaves the message to be
		// retried on the next start rather than sealed half-repaired.
		msg.AddFinish(message.FinishReasonCanceled, interruptedFinishMessage, "")
		if updateErr := messages.Update(ctx, msg); updateErr != nil {
			slog.Error("Failed to close out an interrupted turn",
				"component", "app", "session_id", msg.SessionID, "message_id", msg.ID, "error", updateErr)
		}
	}

	slog.Debug("Closed out interrupted turns from a previous run",
		"component", "app", "project_path", projectPath, "messages", len(unfinished))
	return nil
}

// answeredToolCalls returns the ids of every tool call in sessionID that
// already has a result, so a repair only writes the ones genuinely
// missing. Parallel tool calls make this necessary: a turn can be killed
// with one call answered and another not.
func answeredToolCalls(ctx context.Context, messages message.Service, sessionID string) (map[string]struct{}, error) {
	msgs, err := messages.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	answered := make(map[string]struct{})
	for _, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		for _, tr := range m.ToolResults() {
			answered[tr.ToolCallID] = struct{}{}
		}
	}
	return answered, nil
}

// FinalizeInterruptedTurns is [finalizeInterruptedTurns] for callers
// outside this package, which today means the thread wiring: a thread's
// sessions are recorded under its own worktree, so the sweep Bootstrap
// runs for the workspace being started never reaches them (see
// threadspawn.finalizeThreadTurns, its only caller).
//
// The caller owns the judgement this rests on — that no turn of
// projectPath's is running anywhere. Read that argument in
// finalizeInterruptedTurns before adding a second caller.
func FinalizeInterruptedTurns(ctx context.Context, projectPath string, messages message.Service) error {
	return finalizeInterruptedTurns(ctx, projectPath, messages)
}
