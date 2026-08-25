package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/stringext"
)

// runTurn holds the mutable state a single streaming turn's
// fantasy.AgentStreamCall callbacks read and write. Run constructs one
// per accepted call and wires agent.Stream's callbacks to its methods;
// grouping the state here (instead of ~13 closures over Run's local
// variables) makes the per-callback data flow explicit instead of implicit
// in closure capture.
type runTurn struct {
	agent *sessionAgent
	call  SessionAgentCall

	// turnID is this turn's correlation id, minted once in newRunTurn and
	// shared by every provider request the turn makes (see provider_log.go).
	// A turn is exactly one runTurn, so it is stable for the whole turn even
	// when run_id is empty or the same run_id spans several queued turns.
	turnID string

	// stepNumber/attempt/pendingReason are the mutable per-attempt state that
	// modelProvider folds into a providerCorrelation for the instrumented
	// model (see provider_log.go). stepNumber is the current fantasy step
	// (PrepareStep stamps it); attempt is the 1-based network attempt within
	// the current step (the first is 1, a retry is 2, ...). Both are written
	// by PrepareStep/modelProvider and read by modelProvider, on the stream's
	// own goroutine, so no lock is needed.
	//
	// pendingReason is the request_reason token for the *next* attempt: ""
	// (the baseline turn/continuation cause), reasonRetry (the previous
	// attempt failed transiently and fantasy chose to retry it), or
	// reasonAuthRefresh (the previous attempt failed with an auth error that
	// a successful OnAuthRefresh resolved, and fantasy restarted the pass).
	// It is set by onRetry / onAuthRefresh and consumed (read + cleared) by
	// the next modelProvider call via requestStartReason. Written by the
	// callbacks, read+cleared by modelProvider - all on the stream's
	// goroutine, in the retry loop's own order, so no lock is needed.
	//
	// pendingRetryReason is the filterable retry_reason label the onRetry
	// warning carries (auth / rate_limited / server_error / ...). It is set
	// and logged inside onRetry itself; requestStartReason no longer reads
	// it (pendingReason is the request_reason source), so it is a warning
	// detail, not the retry-pending flag.
	stepNumber         int
	attempt            int
	pendingReason      string
	pendingRetryReason string

	// ctx is Run's outer per-call context (NOT genCtx): several callbacks
	// deliberately use it instead of genCtx so their writes still land
	// even if the request is canceled mid-stream.
	ctx context.Context
	// genCtx is the stream's cancelable context (canceled by Run's defer
	// cancel() or by Cancel()/ctx cancellation).
	genCtx context.Context

	// model is the turn's snapshot of the run's model, taken once in Run
	// and never re-read, so a concurrent SetModel or config reload cannot
	// change this turn's identity mid-stream.
	model                Model
	tools                []fantasy.AgentTool
	promptPrefix         string
	disableAutoSummarize bool

	// userMsgCreated records whether Run already created the turn's user
	// message before entering Stream, for the error path's
	// persistCanceledTurn call.
	userMsgCreated bool

	// sessionLock guards stepMessages and currentSession, both written by
	// PrepareStep/OnStepFinish under it.
	sessionLock    sync.Mutex
	stepMessages   []fantasy.Message
	currentSession session.Session

	sanitizedToolCalls map[string]bool
	shouldSummarize    bool
	// historyTokens is the part of the prompt a summary would replace:
	// this session's own history as it stood when the run started,
	// excluding everything a summary cannot touch (system prompt, skills,
	// carried sub-agent history). See stopOnContextWindow.
	historyTokens int64
	// currentAssistant is the in-flight step's assistant message;
	// PrepareStep (re)assigns it once per streaming step. It's the turn's
	// final assistant message once streaming ends.
	currentAssistant *message.Message

	// pendingCompletions is the batch folded into the current provider step.
	// It remains owned by the turn until OnStepFinish proves that the provider
	// step completed and all local persistence for it succeeded.
	pendingCompletions []TaskCompletion
}

// newRunTurn returns a runTurn ready to be wired into a
// fantasy.AgentStreamCall for the given call.
func newRunTurn(
	a *sessionAgent,
	call SessionAgentCall,
	ctx, genCtx context.Context,
	model Model,
	tools []fantasy.AgentTool,
	promptPrefix string,
	disableAutoSummarize bool,
	currentSession session.Session,
	userMsgCreated bool,
) *runTurn {
	return &runTurn{
		agent:                a,
		call:                 call,
		turnID:               newTurnID(),
		ctx:                  ctx,
		genCtx:               genCtx,
		model:                model,
		tools:                tools,
		promptPrefix:         promptPrefix,
		disableAutoSummarize: disableAutoSummarize,
		userMsgCreated:       userMsgCreated,
		currentSession:       currentSession,
		sanitizedToolCalls:   make(map[string]bool),
	}
}

// prepareStep is the turn's fantasy.PrepareStepFunction. It folds
// completions and queued follow-up prompts into the step (foldCompletions,
// foldSteering), applies cache-control provider options (applyCacheControl),
// and creates the step's assistant message (createStepAssistant).
func (t *runTurn) prepareStep(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
	// Stamp the step for the provider request log lines: the ModelProvider
	// callback (where a request actually starts) does not receive the step
	// number, so it reads the value PrepareStep records here. A fresh step
	// also means the first network attempt of it, so reset the attempt
	// counter to 1 - a retry within the step then reads 2, 3, ...
	t.stepNumber = options.StepNumber
	t.attempt = 0

	prepared.Messages = options.Messages
	for i := range prepared.Messages {
		prepared.Messages[i].ProviderOptions = nil
	}

	prepared.Tools = t.tools

	// A continuation's own call.Prompt is a placeholder
	// (continuationPromptPlaceholder), not real content: it exists only
	// to satisfy fantasy's own createPrompt, which refuses an empty
	// prompt unless the session's prior history already ends in a
	// user/tool message - not the case for a freshly-idle session waking
	// from an ordinary assistant reply. Strip the user message fantasy
	// synthesized from it before the model ever sees it. What replaces it
	// is exactly the completion-inbox drain in foldCompletions - the same
	// mechanism, the same non-persisted delivery, as the mid-turn fold
	// case, so the two paths cannot record the same event differently.
	//
	// stripContinuationPlaceholder verifies the entry it removes actually
	// is the placeholder rather than trusting its position (always last -
	// see fantasy's createPrompt) alone: a future fantasy release that
	// changes how it builds the initial prompt would otherwise either
	// leak the placeholder into the model's context as a fabricated user
	// turn, or delete real content - both silent. A mismatch fails this
	// step loudly instead, at the point the assumption broke.
	if t.call.Continuation && options.StepNumber == 0 {
		prepared.Messages, err = stripContinuationPlaceholder(prepared.Messages)
		if err != nil {
			return callContext, prepared, err
		}
	}

	// A batch folded here is only accepted after the provider step and its
	// local persistence complete in onStepFinish. Preparation failures put it
	// back immediately; provider/stream failures are handled by Run's outer
	// cleanup through requeuePendingCompletions.
	prepared.Messages, t.pendingCompletions = t.foldCompletions(prepared.Messages, options.StepNumber)
	defer func() {
		if err != nil {
			t.requeuePendingCompletions()
		}
	}()

	// foldSteering owns its own rollback: a follow-up it successfully
	// persists via createUserMessage is durable session history the
	// instant it's written, so a later stage's failure has nothing to
	// undo there - only the unpersisted remainder (if persisting one
	// mid-batch fails) goes back to the queue, which foldSteering does
	// itself before returning its error.
	prepared.Messages, err = t.foldSteering(callContext, prepared.Messages)
	if err != nil {
		return callContext, prepared, err
	}

	prepared.Messages = t.applyCacheControl(prepared.Messages)

	callContext, err = t.createStepAssistant(callContext, prepared.Messages)
	return callContext, prepared, err
}

// foldCompletions drains this step's completion inbox and, if anything was
// waiting, appends it to messages as a labeled system-generated report (see
// taskCompletionsMessage) and stamps a continuation's cascade depth from the
// deepest delegation in the batch.
//
// finish is non-nil exactly when something was drained; the caller must
// invoke it exactly once with the step's final error after every later
// stage has run: nothing here persists a completion anywhere durable (unlike
// a folded steering prompt, which is written via createUserMessage before
// it's appended) — a completion lives only in the inbox and, once appended
// here, in this step's in-memory messages. So "delivered" can only be
// declared once the whole step reaches the model; any error from here on
// means it never did, and finish(err) puts the drained batch back rather
// than losing it. On success, finish logs delivery latency (ids, statuses,
// and durations only, never Goal/ResultText/Error).
func (t *runTurn) foldCompletions(messages []fantasy.Message, stepNumber int) ([]fantasy.Message, []TaskCompletion) {
	completions := t.agent.drainCompletionsForStep(t.call.SessionID)
	if len(completions) == 0 {
		return messages, nil
	}
	if t.call.Continuation && stepNumber == 0 {
		// Only knowable now that we see what actually woke this turn.
		// A completion reports the depth of the delegation that produced
		// it, which is one level below the session that started it — and
		// this session is that starter, reading its own delegation's
		// answer. So it is back at its own level, not a deeper one: the
		// deepest completion in the batch, minus one.
		//
		// This is the whole difference between bounding nesting and
		// bounding iteration. Deepening here made a session's *n*th round
		// of delegate-and-react run at depth n, so an ordinary iterative
		// plan — implement, review, implement again — ran out of depth
		// after three rounds and could never delegate again, while a
		// genuinely nested chain of delegations was not counted at all
		// (a delegation's own turn ran at depth 0).
		depth := 0
		for _, c := range completions {
			if c.Depth > depth {
				depth = c.Depth
			}
		}
		t.call.Depth = max(0, depth-1)
	}
	return append(messages, taskCompletionsMessage(completions)), completions
}

func (t *runTurn) requeuePendingCompletions() {
	if len(t.pendingCompletions) == 0 {
		return
	}
	t.agent.requeueCompletions(t.call.SessionID, t.pendingCompletions)
	t.pendingCompletions = nil
}

func (t *runTurn) acknowledgePendingCompletions() {
	completions := t.pendingCompletions
	if len(completions) == 0 {
		return
	}
	ids := make([]string, len(completions))
	waitedMS := make([]int64, len(completions))
	now := time.Now()
	for i, c := range completions {
		ids[i] = c.DelegationID
		waitedMS[i] = now.Sub(c.TerminalAt).Milliseconds()
		t.agent.recordLatency(t.ctx, latency.KindCompletionDelivery, t.call.SessionID, now.Sub(c.TerminalAt))
		if c.Acknowledge != nil {
			if err := c.Acknowledge(context.WithoutCancel(t.ctx)); err != nil {
				slog.Error("Failed to acknowledge completion delivery", "delegation", c.DelegationID, "error", err)
			}
		}
	}
	t.pendingCompletions = nil
	slog.Info("Completion delivered", "session", t.call.SessionID, "delegations", ids, "count", len(completions), "waited_ms", waitedMS)
}

// foldSteering drains queued follow-up prompts for this step and appends
// each as a persisted user message. Calls covered by a cancel recorded
// while they sat in the queue are dropped: a cancel that arrived after a
// prompt was queued must not let it run as part of this step. Coverage is
// per-call by accept sequence so a follow-up queued after the cancel
// (higher seq) is not dropped. A dropped prompt carrying a RunID still gets
// its terminal cancelled RunComplete so a caller waiting on it does not
// hang. Uncanceled prompts without a RunID are folded into this turn;
// uncanceled prompts with a RunID are left queued so each runs as its own
// turn (with its own RunComplete) via run's queue handoff.
func (t *runTurn) foldSteering(callContext context.Context, messages []fantasy.Message) ([]fantasy.Message, error) {
	fold, canceledRunIDs := t.agent.drainQueueForStep(t.call.SessionID)
	t.agent.publishCanceledQueueDrops(canceledRunIDs)
	for i, queued := range fold {
		userMessage, createErr := t.agent.createUserMessage(callContext, queued)
		if createErr != nil {
			// Persisting this follow-up failed. Put it and everything
			// after it (never persisted, never delivered) back at the
			// front of the queue instead of dropping them: they are
			// user-typed steering messages, and losing them silently is
			// worse than leaving them queued for the next step to retry.
			t.agent.requeueDrained(t.call.SessionID, fold[i:])
			return messages, createErr
		}
		messages = append(messages, toAIMessage(&userMessage)...)
	}
	// Every queued call in fold reached this point only once it was
	// successfully persisted above (an error returns early, before this
	// line, with the whole remainder put back in the queue) - so this is
	// the "injection" instant: submit (dispatcher.enqueueCall's stamp)
	// to here is how long a steering message waited to actually reach
	// the model. Calls with no stamp (queuedAt.IsZero()) were queued by
	// a path other than a real steering follow-up - see queuedAt's own
	// doc comment - and are left out rather than reported as a nonsense
	// multi-decade wait.
	if len(fold) > 0 {
		var waitedMS []int64
		now := time.Now()
		for _, queued := range fold {
			if queued.queuedAt.IsZero() {
				continue
			}
			waitedMS = append(waitedMS, now.Sub(queued.queuedAt).Milliseconds())
			t.agent.recordLatency(callContext, latency.KindSteeringFold, t.call.SessionID, now.Sub(queued.queuedAt))
		}
		if len(waitedMS) > 0 {
			slog.Info("Steering folded into turn", "session", t.call.SessionID, "count", len(waitedMS), "waited_ms", waitedMS)
		}
	}
	return messages, nil
}

// applyCacheControl works around provider media limitations, then marks
// provider options for cache-control breakpoints: the last system message
// and the last two messages overall.
func (t *runTurn) applyCacheControl(messages []fantasy.Message) []fantasy.Message {
	messages = t.agent.workaroundProviderMediaLimitations(messages, t.model)

	lastSystemRoleInx := 0
	systemMessageUpdated := false
	for i, msg := range messages {
		// Only add cache control to the last message.
		if msg.Role == fantasy.MessageRoleSystem {
			lastSystemRoleInx = i
		} else if !systemMessageUpdated {
			messages[lastSystemRoleInx].ProviderOptions = t.agent.getCacheControlOptions()
			systemMessageUpdated = true
		}
		// Than add cache control to the last 2 messages.
		if i > len(messages)-3 {
			messages[i].ProviderOptions = t.agent.getCacheControlOptions()
		}
	}

	if t.promptPrefix != "" {
		messages = append([]fantasy.Message{fantasy.NewSystemMessage(t.promptPrefix)}, messages...)
	}
	return messages
}

// createStepAssistant records this step's final message list for
// onStepFinish's usage accounting, creates the step's assistant message,
// and stamps callContext with the values tools read from it for the
// duration of the step.
func (t *runTurn) createStepAssistant(callContext context.Context, messages []fantasy.Message) (context.Context, error) {
	t.sessionLock.Lock()
	t.stepMessages = cloneFantasyMessages(messages)
	t.sessionLock.Unlock()

	assistantMsg, err := t.agent.messages.Create(callContext, t.call.SessionID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    []message.ContentPart{},
		Model:    t.model.ModelCfg.Model,
		Provider: t.model.ModelCfg.Provider,
	})
	if err != nil {
		return callContext, err
	}
	callContext = context.WithValue(callContext, tools.MessageIDContextKey, assistantMsg.ID)
	callContext = context.WithValue(callContext, tools.SupportsImagesContextKey, t.model.CatalogCfg.SupportsImages)
	callContext = context.WithValue(callContext, tools.ModelNameContextKey, t.model.CatalogCfg.Name)
	callContext = context.WithValue(callContext, tools.DepthContextKey, t.call.Depth)
	t.currentAssistant = &assistantMsg
	return callContext, nil
}

func (t *runTurn) onReasoningStart(id string, reasoning fantasy.ReasoningContent) error {
	t.currentAssistant.AppendReasoningContent(reasoning.Text, time.Now().Unix())
	return t.agent.messages.Update(t.genCtx, *t.currentAssistant)
}

func (t *runTurn) onReasoningDelta(id string, text string) error {
	t.currentAssistant.AppendReasoningContent(text, time.Now().Unix())
	return t.agent.messages.Update(t.genCtx, *t.currentAssistant)
}

func (t *runTurn) onReasoningEnd(id string, reasoning fantasy.ReasoningContent) error {
	// handle anthropic signature
	if anthropicData, ok := reasoning.ProviderMetadata[anthropic.Name]; ok {
		if reasoning, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok {
			t.currentAssistant.AppendReasoningSignature(reasoning.Signature)
		}
	}
	if googleData, ok := reasoning.ProviderMetadata[google.Name]; ok {
		if reasoning, ok := googleData.(*google.ReasoningMetadata); ok {
			t.currentAssistant.AppendThoughtSignature(reasoning.Signature, reasoning.ToolID)
		}
	}
	if openaiData, ok := reasoning.ProviderMetadata[openai.Name]; ok {
		if reasoning, ok := openaiData.(*openai.ResponsesReasoningMetadata); ok {
			// message stays free of provider SDK types (see message's
			// ResponsesReasoningMetadata doc comment), so convert here at
			// the provider boundary.
			t.currentAssistant.SetReasoningResponsesData(&message.ResponsesReasoningMetadata{
				ItemID:           reasoning.ItemID,
				EncryptedContent: reasoning.EncryptedContent,
				Summary:          reasoning.Summary,
			})
		}
	}
	t.currentAssistant.FinishThinking(time.Now().Unix())
	return t.agent.messages.Update(t.genCtx, *t.currentAssistant)
}

func (t *runTurn) onTextDelta(id string, text string) error {
	// Strip leading newline from initial text content. This is is
	// particularly important in non-interactive mode where leading
	// newlines are very visible.
	if len(t.currentAssistant.Parts) == 0 {
		text = strings.TrimPrefix(text, "\n")
	}

	t.currentAssistant.AppendContent(text)
	return t.agent.messages.Update(t.genCtx, *t.currentAssistant)
}

func (t *runTurn) onToolInputStart(id string, toolName string) error {
	toolCall := message.ToolCall{
		ID:               id,
		Name:             toolName,
		ProviderExecuted: false,
		Finished:         false,
	}
	t.currentAssistant.AddToolCall(toolCall)
	// Use parent ctx instead of genCtx to ensure the update succeeds
	// even if the request is canceled mid-stream
	return t.agent.messages.Update(t.ctx, *t.currentAssistant)
}

// onToolInputDelta grows the current tool call's arguments as the provider
// streams them, the way onTextDelta grows the assistant's text.
//
// Without this the arguments existed nowhere until the call landed whole:
// onToolInputStart records the call with an empty Input, and the next thing
// to touch it is onToolCall with the finished input. A tool call whose
// arguments are large - a write of a file the model composes in one go -
// therefore leaves the message untouched for as long as it takes to
// generate them, minutes on a local model. Nothing is persisted, so nothing
// is published, and the transcript sits on a bare spinner with no sign that
// work is happening at all. The message layer already expected these
// deltas: shouldFlushNow debounces input growth and flushes on the
// transition to finished (see internal/message).
//
// Deltas are streaming state, so they ride genCtx like the text ones: a
// cancel mid-stream is meant to stop them, and what has to survive it is
// written by the turn's own cleanup with a detached context.
func (t *runTurn) onToolInputDelta(id string, delta string) error {
	t.currentAssistant.AppendToolCallInput(id, delta)
	return t.agent.messages.Update(t.genCtx, *t.currentAssistant)
}

func (t *runTurn) onRetry(err *fantasy.ProviderError, delay time.Duration) {
	// Record why this attempt failed so the next attempt's "provider request
	// started" line is labeled retry, and tag the warning with the turn's
	// correlation ids plus a filterable retry_reason. The failed attempt is
	// the one that just ran, so attempt/step number are still the failed
	// attempt's (modelProvider set them) - this is what ties the warning to
	// the started line it interrupts.
	//
	// pendingReason marks the next attempt as a retry; pendingRetryReason is
	// the filterable label the warning itself carries.
	t.pendingReason = reasonRetry
	t.pendingRetryReason = providerRetryReason(err)
	fields := providerRequestLogFields(t.call.SessionID, t.call.RunID, t.turnID, t.stepNumber, t.attempt, reasonRetry)
	fields = append(fields, "retry_reason", t.pendingRetryReason)
	fields = append(fields, providerRetryLogFields(err, delay)...)
	slog.Warn("Provider request failed, retrying", fields...)
	// Reset streamed content so the retried response doesn't
	// concatenate with partial content from the failed attempt.
	// On the final attempt (no more retries), any partial content
	// stays in the message as useful context beneath the error.
	t.currentAssistant.ResetStreamedContent()
	if updateErr := t.agent.messages.Update(t.genCtx, *t.currentAssistant); updateErr != nil {
		slog.Error("Failed to reset message on retry", "error", updateErr)
	}
}

// onAuthRefresh wraps the call's OnAuthRefresh (the coordinator's credential
// refresh) so a *successful* refresh marks the next attempt as an
// auth_refresh: fantasy restarts the whole retry pass with a fresh budget
// after a refresh, so the first attempt of that pass is this cause, distinct
// from a plain transient retry. A failed refresh does not mark anything -
// fantasy returns the original auth error and no further attempt runs.
//
// It is wired into the turn's AgentStreamCall in Run; the inner callback is
// the one the coordinator supplies (which rebuilds the credentials and swaps
// the active runtime, so the next modelProvider reads the refreshed model).
// The pendingReason write is on the stream's goroutine, in the retry loop's
// order (OnAuthRefresh fires before the restarted pass's first
// ModelProvider), so no lock is needed.
func (t *runTurn) onAuthRefresh(ctx context.Context, err *fantasy.ProviderError) error {
	if t.call.OnAuthRefresh == nil {
		return nil
	}
	if refreshErr := t.call.OnAuthRefresh(ctx, err); refreshErr != nil {
		return refreshErr
	}
	// The refresh succeeded: the next attempt runs with fresh credentials and
	// is labeled auth_refresh rather than a plain retry.
	t.pendingReason = reasonAuthRefresh
	return nil
}

// modelProvider is the turn's fantasy.AgentStreamCall.ModelProvider. It is
// called on each retry attempt, but deliberately returns this turn's runtime
// model rather than the coordinator's mutable agent model. A config reload or
// credential rebuild therefore cannot switch an in-flight turn's identity.
func (t *runTurn) modelProvider() fantasy.LanguageModel {
	m := t.model
	if t.call.ActiveRuntime != nil {
		if runtime := t.call.ActiveRuntime.load(); runtime != nil && runtime.model.ModelCfg.Provider == m.ModelCfg.Provider && runtime.model.ModelCfg.Model == m.ModelCfg.Model {
			m = runtime.model
		}
	}
	// This is fantasy's runtime ModelProvider callback - it fires once per
	// stream attempt. It used to be the Info "ModelProvider called" line the
	// audit flagged as noise (mistaken for the request count, carrying no
	// session/run id or reason); it now goes to Debug. The line that *is*
	// the request count is the "Provider request started" the instrumented
	// model logs, one per real Stream attempt.
	slog.Debug("ModelProvider called",
		"session_id", t.call.SessionID,
		"run_id", t.call.RunID,
		"turn_id", t.turnID,
		"provider", m.ModelCfg.Provider,
		"model", m.ModelCfg.Model)

	// Count the attempt and wrap the real model so its Stream is logged as a
	// started/finished pair. attempt is 1-based within the current step (a
	// retry reads 2, 3, ...); requestStartReason prefers an in-flight retry
	// (recorded by onRetry) over the step's baseline cause. The instrumented
	// model owns both halves of the pair - started here, finished when the
	// Stream terminates - so a failed-then-retried attempt and a multi-attempt
	// step each produce a complete, 1:1 pair instead of an orphaned started.
	t.attempt++
	corr := providerCorrelation{
		sessionID: t.call.SessionID,
		runID:     t.call.RunID,
		turnID:    t.turnID,
		step:      t.stepNumber,
		attempt:   t.attempt,
		reason:    t.requestStartReason(),
	}
	return newInstrumentedModel(m.Model, corr, m.ModelCfg.Provider)
}

// requestStartReason labels the request about to be made. It prefers a
// pending re-attempt cause (set by onRetry -> retry, or by a successful
// onAuthRefresh -> auth_refresh) over the step's baseline cause, so a step's
// log stream reads "started(turn)" then "started(retry)" or
// "started(auth_refresh)" for its attempts. The baseline is a continuation
// when the turn auto-woke from the inbox, otherwise an ordinary turn step. A
// summarize pass never runs through this path - it is its own agent.Stream in
// usage.go, labeled there by its own state (summary / retry / auth_refresh).
func (t *runTurn) requestStartReason() string {
	if t.pendingReason != "" {
		reason := t.pendingReason
		t.pendingReason = ""
		return reason
	}
	if t.call.Continuation {
		return reasonContinuation
	}
	return reasonTurn
}

func (t *runTurn) onToolCall(tc fantasy.ToolCallContent) error {
	// Lifecycle logs deliberately contain correlation only: tool arguments may
	// contain paths, source, credentials, or other user data.
	slog.Info("Tool lifecycle", "session_id", t.call.SessionID, "run_id", t.call.RunID,
		"turn_id", t.turnID, "tool_call_id", tc.ToolCallID, "tool_name", tc.ToolName,
		"event", "tool_call", "tool_outcome", "started")
	input, wasSanitized := sanitizeToolInput(tc.ToolName, tc.ToolCallID, tc.Input)
	if wasSanitized {
		t.sanitizedToolCalls[tc.ToolCallID] = true
	}
	toolCall := message.ToolCall{
		ID:               tc.ToolCallID,
		Name:             tc.ToolName,
		Input:            input,
		ProviderExecuted: false,
		Finished:         true,
	}
	t.currentAssistant.AddToolCall(toolCall)
	// Use parent ctx instead of genCtx to ensure the update succeeds
	// even if the request is canceled mid-stream
	return t.agent.messages.Update(t.ctx, *t.currentAssistant)
}

func (t *runTurn) onToolResult(result fantasy.ToolResultContent) error {
	outcome := "success"
	if result.Result.GetType() == fantasy.ToolResultContentTypeError {
		outcome = "error"
	}
	// Do not log result content: it can include file data or command output.
	slog.Info("Tool lifecycle", "session_id", t.call.SessionID, "run_id", t.call.RunID,
		"turn_id", t.turnID, "tool_call_id", result.ToolCallID, "tool_name", result.ToolName,
		"event", "tool_result", "tool_outcome", outcome)
	toolResult := t.agent.convertToToolResult(result)
	if t.sanitizedToolCalls[result.ToolCallID] {
		toolResult.Content = "Tool call failed: arguments were not valid JSON. Please check your tool call format and try again."
		toolResult.IsError = true
	}
	// Use parent ctx instead of genCtx to ensure the message is created
	// even if the request is canceled mid-stream
	_, createMsgErr := t.agent.messages.Create(t.ctx, t.currentAssistant.SessionID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			toolResult,
		},
	})
	return createMsgErr
}

func (t *runTurn) onStepFinish(stepResult fantasy.StepResult) error {
	for _, w := range stepResult.Warnings {
		slog.Warn("Provider warning", "type", w.Type, "message", w.Message)
	}
	finishReason := message.FinishReasonUnknown
	switch stepResult.FinishReason {
	case fantasy.FinishReasonLength:
		finishReason = message.FinishReasonMaxTokens
	case fantasy.FinishReasonStop:
		finishReason = message.FinishReasonEndTurn
	case fantasy.FinishReasonToolCalls:
		finishReason = message.FinishReasonToolUse
	case fantasy.FinishReasonContentFilter:
		// Provider safety classifier stopped the model
		// (Anthropic stop_reason=refusal, OpenAI content_filter).
		// The TUI owns the display copy; we only persist the
		// reason so the UI can show a REFUSED banner.
		finishReason = message.FinishReasonContentFilter
		slog.Warn(
			"Provider content filter stopped the model",
			"session_id", t.call.SessionID,
			"finish_reason", string(stepResult.FinishReason),
		)
	}
	// If a tool result halted the turn (e.g. a hook halt or a
	// permission denial), the step ends on FinishReasonToolCalls but
	// the model will not be called again. Treat it as the end of the
	// turn so the UI can render the assistant footer.
	if finishReason == message.FinishReasonToolUse {
		for _, tr := range stepResult.Content.ToolResults() {
			if tr.StopTurn {
				finishReason = message.FinishReasonEndTurn
				break
			}
		}
	}
	t.currentAssistant.AddFinish(finishReason, time.Now().Unix(), "", "")
	// The provider request's "finished" line is logged by the instrumented
	// model that performed this step's Stream (it is the 1:1 counterpart of
	// the "started" line, and it is what carries finish_reason + usage). We
	// do NOT log a second finished line here - that was the duplicate the
	// audit caught. onStepFinish still owns the session usage accounting and
	// latency recording below.
	t.sessionLock.Lock()
	defer t.sessionLock.Unlock()

	updatedSession, getSessionErr := t.agent.sessions.Get(t.ctx, t.call.SessionID)
	if getSessionErr != nil {
		return getSessionErr
	}
	usage, estimated := fallbackStepUsage(t.stepMessages, stepResult)
	t.agent.updateSessionUsage(t.model, &updatedSession, usage, t.agent.openrouterCost(stepResult.ProviderMetadata), estimated)
	_, sessionErr := t.agent.sessions.Save(t.ctx, updatedSession)
	if sessionErr != nil {
		return sessionErr
	}
	t.currentSession = updatedSession
	if err := t.agent.messages.Update(t.genCtx, *t.currentAssistant); err != nil {
		return err
	}
	t.acknowledgePendingCompletions()
	return nil
}

// maxOutputTokens is the largest reply this turn's model may produce. Zero
// means unknown, which summarizeBuffer reads as "use the standard buffer".
func (t *runTurn) maxOutputTokens() int64 {
	return modelMaxOutputTokens(t.model)
}

// stopOnContextWindow is the auto-summarize StopWhen condition: it stops
// the turn once the session's token usage crosses the context-window
// threshold, so Run's tail can kick off a summarize pass.
func (t *runTurn) stopOnContextWindow(_ []fantasy.StepResult) bool {
	cw := t.model.CatalogCfg.ContextWindow
	// If context window is unknown (0), skip auto-summarize
	// to avoid immediately truncating custom/local models.
	if cw == 0 {
		return false
	}
	tokens := t.currentSession.CompletionTokens + t.currentSession.PromptTokens
	remaining := cw - tokens
	threshold := summarizeBuffer(cw, t.maxOutputTokens())
	if remaining > threshold || t.disableAutoSummarize {
		return false
	}
	// Summarizing only helps if what it reclaims is big enough to matter.
	// A summary replaces this session's history and nothing else: the
	// system prompt, the skills, any carried sub-agent history and the
	// summary a previous pass already wrote all come back untouched. When
	// the history is itself smaller than the buffer we are trying to
	// free, another pass reads the whole context to write a summary that
	// cannot change the outcome, and the next continuation trips at once
	// — the session summarizes on a loop instead of working. Run on
	// instead and let the provider's own limit speak.
	if t.historyTokens > 0 && t.historyTokens < threshold {
		slog.Warn(
			"Skipping auto-summarize: the session history is smaller than the room a summary would need to free",
			"session_id", t.call.SessionID,
			"context_window", cw,
			"tokens", tokens,
			"history_tokens", t.historyTokens,
			"threshold", threshold,
		)
		return false
	}
	slog.Info(
		"Auto-summarize threshold reached",
		"session_id", t.call.SessionID,
		"context_window", cw,
		"tokens", tokens,
		"history_tokens", t.historyTokens,
		"threshold", threshold,
	)
	t.shouldSummarize = true
	return true
}

// handleStreamError implements Run's post-Stream error path: it finalizes
// the assistant message (closing out any unfinished tool calls with a
// synthetic error result), records a finish reason describing the error,
// and persists the result with a context detached from the canceled run
// context.
func (t *runTurn) handleStreamError(err error) (*fantasy.AgentResult, error) {
	isCancelErr := errors.Is(err, context.Canceled)
	slog.Info("Agent stream returned error",
		"error", err.Error(),
		"error_type", fmt.Sprintf("%T", err),
		"is_cancel", isCancelErr)
	if t.currentAssistant == nil {
		// Cancel-before-assistant-creation window: the run was
		// canceled after activeRequests.Set but before PrepareStep
		// created the assistant message. Without this, the turn
		// would return with no FinishReasonCanceled marker and no
		// user-visible record. The user message was already created
		// above, so persistCanceledTurn only writes the assistant
		// record.
		if isCancelErr {
			if persistErr := t.agent.persistCanceledTurn(t.ctx, t.call, t.userMsgCreated); persistErr != nil {
				return nil, persistErr
			}
		}
		return nil, err
	}
	// Persist final state with a context detached from the run
	// context. The run context (ctx) is derived from the
	// workspace context, which workspace shutdown cancels before
	// agent goroutines finish; using ctx here would drop the
	// final assistant state. WithoutCancel keeps the values
	// (e.g. session ID) while ignoring cancellation, and a short
	// timeout bounds the cleanup writes.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(t.ctx), 5*time.Second)
	defer cleanupCancel()
	// Ensure we finish thinking on error to close the reasoning state.
	t.currentAssistant.FinishThinking(time.Now().Unix())
	toolCalls := t.currentAssistant.ToolCalls()
	// INFO: we use the cleanup context here because the genCtx has been cancelled.
	//
	// A failure to read the session back must not abandon the rest of this
	// cleanup. Bailing out here used to skip both the Finished flags below and
	// the AddFinish further down, leaving a persisted assistant message with
	// neither — a state the chat UI reads as "still running" for the rest of
	// that session's life, every time it is reloaded. Without the list we
	// cannot tell which calls already have results, so only the synthetic ones
	// are skipped; compat.go injects those at prompt-build time regardless, so
	// the conversation still cannot lock on an orphaned call.
	msgs, listErr := t.agent.messages.List(cleanupCtx, t.currentAssistant.SessionID)
	if listErr != nil {
		slog.Error(
			"Failed to read session messages while closing an interrupted turn, skipping synthetic tool results",
			"session_id", t.currentAssistant.SessionID,
			"error", listErr,
		)
	}
	for _, tc := range toolCalls {
		if !tc.Finished {
			tc.Finished = true
			tc.Input = "{}"
			t.currentAssistant.AddToolCall(tc)
			// Not fatal either: the Update that follows AddFinish below
			// persists this same message, so one failed write here must not
			// cost the turn its finish reason.
			if updateErr := t.agent.messages.Update(cleanupCtx, *t.currentAssistant); updateErr != nil {
				slog.Error(
					"Failed to persist a closed tool call while closing an interrupted turn",
					"session_id", t.currentAssistant.SessionID,
					"tool_call_id", tc.ID,
					"error", updateErr,
				)
			}
		}
		if listErr != nil {
			continue
		}

		found := false
		for _, msg := range msgs {
			if msg.Role == message.Tool {
				for _, tr := range msg.ToolResults() {
					if tr.ToolCallID == tc.ID {
						found = true
						break
					}
				}
			}
			if found {
				break
			}
		}
		if found {
			continue
		}
		content := "There was an error while executing the tool"
		if isCancelErr {
			content = "Error: user cancelled assistant tool calling"
		}
		toolResult := message.ToolResult{
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    content,
			IsError:    true,
		}
		_, createErr := t.agent.messages.Create(cleanupCtx, t.currentAssistant.SessionID, message.CreateMessageParams{
			Role: message.Tool,
			Parts: []message.ContentPart{
				toolResult,
			},
		})
		if createErr != nil {
			return nil, createErr
		}
	}
	var fantasyErr *fantasy.Error
	var providerErr *fantasy.ProviderError
	const defaultTitle = "Provider Error"
	if isCancelErr {
		t.currentAssistant.AddFinish(message.FinishReasonCanceled, time.Now().Unix(), "User canceled request", "")
	} else if errors.As(err, &providerErr) {
		// classModelNotEnabled is a generic "model not enabled" signal
		// from classifyStreamError; only Copilot's rejection actually
		// means what the message below says, so gate on the provider
		// before treating every provider's not-enabled error as a
		// Copilot quota problem with a link to Copilot's own settings.
		if t.model.ModelCfg.Provider == string(catwalk.InferenceProviderCopilot) && classifyStreamError(providerErr) == classModelNotEnabled {
			// The TUI owns the display copy: return a typed error so
			// callers can render their own styled hyperlink instead of
			// agent baking terminal escape codes into persisted
			// message content.
			quotaErr := &ProviderQuotaError{
				Provider:    "copilot",
				Model:       t.model.CatalogCfg.Name,
				SettingsURL: "https://github.com/settings/copilot/features",
			}
			t.currentAssistant.AddFinish(
				message.FinishReasonError,
				time.Now().Unix(),
				"Copilot model not enabled",
				fmt.Sprintf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", quotaErr.Model, quotaErr.SettingsURL),
			)
			err = quotaErr
		} else {
			t.currentAssistant.AddFinish(message.FinishReasonError, time.Now().Unix(), cmp.Or(stringext.Capitalize(providerErr.Title), defaultTitle), providerErr.Message)
		}
	} else if errors.As(err, &fantasyErr) {
		t.currentAssistant.AddFinish(message.FinishReasonError, time.Now().Unix(), cmp.Or(stringext.Capitalize(fantasyErr.Title), defaultTitle), fantasyErr.Message)
	} else if fantasy.IsTransportError(err) {
		wrapped := fantasy.NewTransportError(err)
		t.currentAssistant.AddFinish(message.FinishReasonError, time.Now().Unix(), stringext.Capitalize(wrapped.Title), wrapped.Message)
	} else {
		t.currentAssistant.AddFinish(message.FinishReasonError, time.Now().Unix(), defaultTitle, err.Error())
	}
	// Note: we use the cleanup context here because the genCtx has been
	// cancelled.
	updateErr := t.agent.messages.Update(cleanupCtx, *t.currentAssistant)
	if updateErr != nil {
		return nil, updateErr
	}
	return nil, err
}
