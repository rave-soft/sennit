package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
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
func finalizeInterruptedTurns(ctx context.Context, projectPath string, messages messagestore.Service) error {
	unfinished, err := messages.ListUnfinishedAssistantMessages(ctx, projectPath)
	if err != nil {
		return err
	}
	return finalizeInterruptedMessages(ctx, unfinished, messages)
}

func finalizeResumedDelegation(ctx context.Context, queries *db.Queries, projectPath, workingDir, delegationID, sessionID string, messages messagestore.Service) error {
	if delegationID == "" || sessionID == "" {
		return fmt.Errorf("delegation and session IDs are required")
	}
	delegation, err := queries.GetThread(ctx, delegationID)
	if err != nil {
		return fmt.Errorf("get resumed delegation: %w", err)
	}
	if delegation.ProjectPath != projectPath || delegation.SessionID != sessionID || delegation.ParentSessionID == "" || fsext.Canonical(delegation.WorktreePath) != fsext.Canonical(workingDir) {
		return fmt.Errorf("resumed delegation ownership does not match project, worktree, and session")
	}
	sess, err := queries.GetSessionByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get resumed session: %w", err)
	}
	if sess.ProjectPath != projectPath || !sess.ParentSessionID.Valid || sess.ParentSessionID.String != delegation.ParentSessionID {
		return fmt.Errorf("resumed session ownership does not match delegation")
	}
	all, err := messages.List(ctx, sessionID)
	if err != nil {
		return err
	}
	unfinished := make([]message.Message, 0, 1)
	for _, msg := range all {
		if msg.Role == message.Assistant && msg.FinishPart() == nil {
			unfinished = append(unfinished, msg)
		}
	}
	return finalizeInterruptedMessages(ctx, unfinished, messages)
}

func finalizeInterruptedMessages(ctx context.Context, unfinished []message.Message, messages messagestore.Service) error {
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
			var err error
			seen, err = answeredToolCalls(ctx, messages, msg.SessionID)
			if err != nil {
				slog.Error("Failed to read a session while closing out an interrupted turn",
					"component", "app", "session_id", msg.SessionID, "error", err)
				continue
			}
			answered[msg.SessionID] = seen
		}

		repaired := true
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
				repaired = false
				continue
			}
			// Recorded, so a sibling unfinished message in the same
			// session does not answer it a second time.
			seen[tc.ID] = struct{}{}
		}
		if !repaired {
			// A tool call was left without a result, so this message must
			// not look repaired: sealing it here would take it out of
			// ListUnfinishedAssistantMessages' reach forever, leaving that
			// call's result missing for good. Skip the seal so the next
			// start finds this message unfinished and tries again.
			continue
		}

		// The Finish is what takes the message out of this query's reach,
		// so it is written last: an error above leaves the message to be
		// retried on the next start rather than sealed half-repaired.
		msg.AddFinish(message.FinishReasonCanceled, time.Now().Unix(), interruptedFinishMessage, "")
		if updateErr := messages.Update(ctx, msg); updateErr != nil {
			slog.Error("Failed to close out an interrupted turn",
				"component", "app", "session_id", msg.SessionID, "message_id", msg.ID, "error", updateErr)
		}
	}

	slog.Debug("Closed out interrupted turns from a previous run",
		"component", "app", "messages", len(unfinished))
	return nil
}

// answeredToolCalls returns the ids of every tool call in sessionID that
// already has a result, so a repair only writes the ones genuinely
// missing. Parallel tool calls make this necessary: a turn can be killed
// with one call answered and another not.
func answeredToolCalls(ctx context.Context, messages messagestore.Service, sessionID string) (map[string]struct{}, error) {
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
