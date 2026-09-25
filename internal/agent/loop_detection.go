package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/message"
)

const (
	loopDetectionWindowSize = 10
	loopDetectionMaxRepeats = 5
)

// repeatedToolCall looks for a tool interaction the agent is stuck
// repeating. It examines the last loopDetectionWindowSize steps and
// returns the first signature, in step order, that appears more than
// loopDetectionMaxRepeats times, with its count; "" when there is none.
//
// skip, when non-nil, leaves out signatures the caller has already dealt
// with: a loop the model was warned about still fills the window for
// several steps after the warning, and would otherwise hide a second,
// different loop behind it.
func repeatedToolCall(steps []fantasy.StepResult, skip func(sig string) bool) (string, int) {
	if len(steps) < loopDetectionWindowSize {
		return "", 0
	}

	window := steps[len(steps)-loopDetectionWindowSize:]
	counts := make(map[string]int)

	for _, step := range window {
		sig := getToolInteractionSignature(step.Content)
		if sig == "" || (skip != nil && skip(sig)) {
			continue
		}
		counts[sig]++
		if counts[sig] > loopDetectionMaxRepeats {
			return sig, counts[sig]
		}
	}

	return "", 0
}

// toolCallNames lists the tools a step called, in call order, for log
// lines and the notices built from a loop.
func toolCallNames(content fantasy.ResponseContent) []string {
	calls := content.ToolCalls()
	names := make([]string, len(calls))
	for i, tc := range calls {
		names[i] = tc.ToolName
	}
	return names
}

// toolLoopWarningPrefix opens the warning a looping turn hands the model,
// for the same reason message.DelegationReportPrefix opens a report: the
// warning is persisted as a user-role message, and the model must not read
// it as something the person typed.
const toolLoopWarningPrefix = "[system-generated loop warning - not user input]"

// Title of the banner a turn stopped on a loop ends with. The details
// line under it is built per stop; see recordToolLoopStop.
const toolLoopStopTitle = "Stopped: the model kept repeating the same tool call"

// toolLoop describes one detected loop: the tools the repeated step
// called and how many times the identical step occurred.
type toolLoop struct {
	tools   []string
	repeats int
}

func (l toolLoop) toolList() string {
	return strings.Join(l.tools, ", ")
}

// stopOnToolLoop is the loop-detection StopWhen condition. A loop is first
// answered with a warning, not a stop: the condition schedules one for the
// next step (see injectToolLoopWarning) and lets the turn go on. Weaker
// models often leave the loop once told they are in one, and stopping
// outright ended the turn with nothing said to the model or the person.
// The turn is stopped only when the model makes the step it was warned
// about once more.
//
// Each loop is warned about once per turn, keyed by its signature, and
// stopped the first time it recurs after that warning. A different loop
// later in the same turn gets a warning of its own.
func (t *runTurn) stopOnToolLoop(steps []fantasy.StepResult) bool {
	if len(steps) == 0 {
		return false
	}
	last := steps[len(steps)-1]
	if sig := getToolInteractionSignature(last.Content); sig != "" {
		if warnedAt, warned := t.loopWarned[sig]; warned && len(steps) > warnedAt {
			repeats := 0
			for _, step := range steps {
				if getToolInteractionSignature(step.Content) == sig {
					repeats++
				}
			}
			t.toolLoopStop = &toolLoop{tools: toolCallNames(last.Content), repeats: repeats}
			slog.Warn("Stopping turn: the model repeated a tool call after being warned about the loop",
				"session_id", t.call.SessionID,
				"run_id", t.call.RunID,
				"turn_id", t.turnID,
				"step", len(steps)-1,
				"tools", t.toolLoopStop.tools,
				"repeats", repeats,
			)
			return true
		}
	}

	sig, repeats := repeatedToolCall(steps, func(sig string) bool {
		_, warned := t.loopWarned[sig]
		return warned
	})
	if sig == "" {
		return false
	}
	var tools []string
	for i := len(steps) - 1; i >= 0; i-- {
		if getToolInteractionSignature(steps[i].Content) == sig {
			tools = toolCallNames(steps[i].Content)
			break
		}
	}
	if t.loopWarned == nil {
		t.loopWarned = make(map[string]int)
	}
	t.loopWarned[sig] = len(steps)
	t.loopWarning = &toolLoop{tools: tools, repeats: repeats}
	slog.Warn("Repeated tool calls detected: warning the model before stopping the turn",
		"session_id", t.call.SessionID,
		"run_id", t.call.RunID,
		"turn_id", t.turnID,
		"step", len(steps)-1,
		"tools", tools,
		"repeats", repeats,
		"window", loopDetectionWindowSize,
	)
	return false
}

// injectToolLoopWarning appends the warning stopOnToolLoop scheduled, if
// any, to the step about to be sent. It is persisted first, as a
// user-role message with Origin agent, so the transcript shows the person
// why the model changed course and later turns keep the record. Within
// this turn the model sees it on this one step, the way a folded steering
// prompt is seen.
func (t *runTurn) injectToolLoopWarning(ctx context.Context, messages []fantasy.Message) ([]fantasy.Message, error) {
	if t.loopWarning == nil {
		return messages, nil
	}
	loop := *t.loopWarning
	t.loopWarning = nil
	warning, err := t.agent.messages.Create(ctx, t.call.SessionID, message.CreateMessageParams{
		Role:   message.User,
		Parts:  []message.ContentPart{message.TextContent{Text: toolLoopWarningText(loop)}},
		Origin: message.OriginAgent,
	})
	if err != nil {
		return messages, fmt.Errorf("failed to persist tool loop warning: %w", err)
	}
	return append(messages, toAIMessage(&warning)...), nil
}

func toolLoopWarningText(loop toolLoop) string {
	return fmt.Sprintf("%s\n"+
		"In your last %d steps you made the same %s call %d times, with the same arguments, and got the same result each time. "+
		"Calling it again will not produce anything new.\n"+
		"Use the result you already have, take a different approach, or stop and report what is blocking you. "+
		"If you make this exact call again, the turn will be stopped.",
		toolLoopWarningPrefix, loopDetectionWindowSize, loop.toolList(), loop.repeats)
}

// toolLoopStopError is the error a turn stopped on a loop reports on its
// RunComplete. internal/thread reads it to finalize a delegation, so the
// parent gets a failure with its cause instead of an empty success.
func (t *runTurn) toolLoopStopError() string {
	return fmt.Sprintf("%s: %s repeated %d times with the same arguments and result",
		toolLoopStopTitle, t.toolLoopStop.toolList(), t.toolLoopStop.repeats)
}

// recordToolLoopStop marks the turn's last assistant message with
// FinishReasonToolLoop once stopOnToolLoop has ended the turn. That step
// finished normally (tool calls, results and all) and onStepFinish already
// stamped it FinishReasonToolUse, which reads as a turn about to continue;
// without this the transcript showed a turn that simply stopped.
//
// A failed write is logged, not returned: the turn itself completed, and
// the log line from stopOnToolLoop already carries the cause.
func (t *runTurn) recordToolLoopStop() {
	if t.toolLoopStop == nil || t.currentAssistant == nil {
		return
	}
	details := fmt.Sprintf("The model called %s with the same arguments and got the same result %d times, "+
		"and repeated it again after being warned. Send a message to continue.",
		t.toolLoopStop.toolList(), t.toolLoopStop.repeats)
	t.currentAssistant.AddFinish(message.FinishReasonToolLoop, time.Now().Unix(), toolLoopStopTitle, details)
	if err := t.agent.messages.Update(context.WithoutCancel(t.genCtx), *t.currentAssistant); err != nil {
		slog.Error("Failed to record a tool loop stop on the assistant message",
			"session_id", t.call.SessionID, "message_id", t.currentAssistant.ID, "error", err)
	}
}

// getToolInteractionSignature computes a hash signature for the tool
// interactions in a single step's content. It pairs tool calls with their
// results (matched by ToolCallID) and returns a hex-encoded SHA-256 hash.
// If the step contains no tool calls, it returns "".
func getToolInteractionSignature(content fantasy.ResponseContent) string {
	toolCalls := content.ToolCalls()
	if len(toolCalls) == 0 {
		return ""
	}

	// Index tool results by their ToolCallID for fast lookup.
	resultsByID := make(map[string]fantasy.ToolResultContent)
	for _, tr := range content.ToolResults() {
		resultsByID[tr.ToolCallID] = tr
	}

	h := sha256.New()
	for _, tc := range toolCalls {
		output := ""
		if tr, ok := resultsByID[tc.ToolCallID]; ok {
			output = toolResultOutputString(tr.Result)
		}
		_, _ = io.WriteString(h, tc.ToolName)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, tc.Input)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, output)
		_, _ = io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// toolResultOutputString converts a ToolResultOutputContent to a stable string
// representation for signature comparison.
func toolResultOutputString(result fantasy.ToolResultOutputContent) string {
	if result == nil {
		return ""
	}
	if text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result); ok {
		return text.Text
	}
	if errResult, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result); ok {
		if errResult.Error != nil {
			return errResult.Error.Error()
		}
		return ""
	}
	if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result); ok {
		return media.Data
	}
	return ""
}
