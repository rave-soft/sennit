package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/shell"
)

// -- Agent --

// AgentRun dispatches a prompt fire-and-forget through the App's
// AgentDispatcher and returns as soon as it is accepted: structural/refusal
// errors (empty prompt, missing session, an uninitialized coordinator, or a
// dispatcher already closing) come back synchronously, while a failure
// in the turn itself reaches observers later as a notify.TypeAgentError
// notification instead of through this return value. The interactive
// TUI does not consume notify.RunComplete for completion detection (it
// observes message events directly), so passing an empty RunID is
// correct here: it skips the correlator stamping path without
// functional consequences.
func (w *AppWorkspace) AgentRun(_ context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	dispatcher := w.app.AgentDispatcher()
	if dispatcher == nil {
		// Only reachable for a hand-built *app.App that skipped both
		// New and NewForTest (every real construction path sets a
		// dispatcher unconditionally); guard it explicitly rather than
		// dereferencing a nil pointer.
		return app.ErrCoordinatorNotInitialized
	}
	return dispatcher.Send(sessionID, "", prompt, attachments)
}

func (w *AppWorkspace) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	var persist shell.PersistFunc
	if sessionID != "" {
		persist = func(cmd, output string, exitCode int) error {
			return persistShellOutput(ctx, w.app.Messages(), sessionID, cmd, output, exitCode)
		}
	}

	opts := shell.RunOptions{
		Command:   command,
		Cwd:       w.store.WorkingDir(),
		TermWidth: termWidth,
	}

	var result shell.CaptureResult
	var err error

	if onProgress != nil {
		result, err = runAndCaptureStream(ctx, opts, onProgress)
	} else {
		result, err = runAndPersist(ctx, opts, persist)
	}

	// Both paths report failure the same way: skip persistence and
	// surface the error, matching RunAndPersist's own convention of
	// returning early on error without calling persist. A streamed
	// command that fails to start left result at its zero value, and
	// persisting a zero/partial result under this command's name would
	// misrepresent it as having produced empty, successful output.
	if err != nil {
		return proto.ShellCommandResponse{}, err
	}

	// Persist if we used the streaming path (persist wasn't called by RunAndPersist).
	if onProgress != nil && persist != nil {
		if persistErr := persist(command, result.Output, result.ExitCode); persistErr != nil {
			slog.Error("Failed to persist shell command output", "error", persistErr, "command", command)
		}
	}

	// Generate a title from the shell command if it was the first message.
	if isFirstMessage {
		if coord := w.app.Coordinator(); coord != nil {
			titleCtx := context.WithoutCancel(ctx)
			coord.GenerateTitle(titleCtx, sessionID, "$ "+command)
		}
	}

	return proto.ShellCommandResponse{
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, nil
}

func (w *AppWorkspace) AgentCancel(sessionID string) {
	if coord := w.app.Coordinator(); coord != nil {
		coord.Cancel(sessionID)
	}
}

func (w *AppWorkspace) AgentIsBusy() bool {
	coord := w.app.Coordinator()
	if coord == nil {
		return false
	}
	return coord.IsBusy()
}

func (w *AppWorkspace) AgentIsSessionBusy(sessionID string) bool {
	coord := w.app.Coordinator()
	if coord == nil {
		return false
	}
	return coord.IsSessionBusy(sessionID)
}

// AgentModel reports the model the next turn will run on, which is the
// one the config selects.
//
// It deliberately does not ask the coordinator, whose Model() returns a
// copy held on the current agent and written only by UpdateModels. A run
// never reads that copy: coordinator.run resolves its own runtime from the
// config on every dispatch (the runtime cache is keyed on the config
// version, so a model change rebuilds it). The two therefore diverge
// whenever an UpdateModels does not land — and when they diverged, the
// display sat on the previous model while every answer came back from the
// new one, with no way for the user to tell which was true.
//
// The coordinator remains the fallback for a config that selects nothing,
// which is the state before onboarding picks a model.
func (w *AppWorkspace) AgentModel() AgentModel {
	coord := w.app.Coordinator()
	if coord == nil {
		return AgentModel{}
	}
	cfg := w.store.Config()
	if cfg == nil || cfg.Model.Model == "" {
		m := coord.Model()
		return AgentModel{CatalogCfg: m.CatalogCfg, ModelCfg: m.ModelCfg}
	}
	selected := cfg.Model
	// A model the catalog cannot resolve is still named by its id rather
	// than left blank: "which model is this" is the question the sidebar
	// exists to answer, and the id answers it better than an empty line.
	catalog := catwalk.Model{ID: selected.Model, Name: selected.Model}
	// A config with no providers can resolve nothing, and GetModel reads
	// the map without checking.
	if cfg.Providers != nil {
		if known := cfg.GetModel(selected.Provider, selected.Model); known != nil {
			catalog = *known
		}
	}
	return AgentModel{CatalogCfg: catalog, ModelCfg: selected}
}

func (w *AppWorkspace) AgentIsReady() bool {
	return w.app.Coordinator() != nil
}

func (w *AppWorkspace) AgentReadyErr() error {
	if w.app.Coordinator() == nil {
		return ErrAgentNotInitialized
	}
	return nil
}

func (w *AppWorkspace) AgentQueuedPrompts(sessionID string) int {
	coord := w.app.Coordinator()
	if coord == nil {
		return 0
	}
	return coord.QueuedPrompts(sessionID)
}

func (w *AppWorkspace) AgentQueuedPromptsList(sessionID string) []string {
	coord := w.app.Coordinator()
	if coord == nil {
		return nil
	}
	return coord.QueuedPromptsList(sessionID)
}

func (w *AppWorkspace) AgentClearQueue(sessionID string) {
	if coord := w.app.Coordinator(); coord != nil {
		coord.ClearQueue(sessionID)
	}
}

func (w *AppWorkspace) AgentSummarize(ctx context.Context, sessionID string) error {
	coord := w.app.Coordinator()
	if coord == nil {
		return errors.New("agent coordinator not initialized")
	}
	return coord.Summarize(ctx, sessionID)
}

func (w *AppWorkspace) UpdateAgentModel(ctx context.Context) error {
	return w.app.UpdateAgentModel(ctx)
}

// ApplySessionModel implements AgentController. See the interface for the
// contract; the checks below are in the order that makes each subsequent
// one meaningful.
func (w *AppWorkspace) ApplySessionModel(ctx context.Context, sessionID string) (bool, error) {
	if w.app.Coordinator() == nil {
		return false, ErrAgentNotInitialized
	}
	sess, err := w.app.Sessions().Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if sess.Model.IsZero() {
		return false, nil
	}
	pinned := config.SelectedModel{Provider: sess.Model.Provider, Model: sess.Model.Model}

	cfg := w.store.Config()
	current := cfg.Model
	if current.Provider == pinned.Provider && current.Model == pinned.Model {
		return false, nil
	}

	// The pin outlives the configuration that made it valid: a provider
	// can be removed, or the session can have been recorded on a different
	// machine entirely. Verify before switching, because switching onto a
	// model this instance cannot build fails every turn in the session
	// rather than the one selection — which is exactly the failure the
	// fallback to the instance's own model exists to avoid.
	providerCfg, ok := cfg.Providers.Get(pinned.Provider)
	if !ok {
		slog.Debug("Session pins a model whose provider is not configured here; keeping the current model",
			"session_id", sessionID, "provider", pinned.Provider, "model", pinned.Model)
		return false, nil
	}
	if !slices.ContainsFunc(providerCfg.Models, func(m catwalk.Model) bool { return m.ID == pinned.Model }) {
		slog.Debug("Session pins a model its provider no longer offers; keeping the current model",
			"session_id", sessionID, "provider", pinned.Provider, "model", pinned.Model)
		return false, nil
	}

	// Carry the current selection's per-model preferences across rather
	// than resetting them: the pin records which model the session ran on,
	// not how the user had it tuned.
	pinned.Think = current.Think
	pinned.MaxTokens = current.MaxTokens
	pinned.ReasoningEffort = current.ReasoningEffort

	w.store.OverridePreferredModel(pinned)
	if err := w.app.UpdateAgentModel(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (w *AppWorkspace) InitCoderAgent(ctx context.Context) error {
	return w.app.InitCoderAgent(ctx)
}

func (w *AppWorkspace) InitCoderAgentNonInteractive(ctx context.Context) error {
	return w.app.InitCoderAgentNonInteractive(ctx)
}

// AgentRunStream starts a non-interactive turn against the local
// agent coordinator and streams it to completion. This is the
// run-loop body that used to live in app.App.RunNonInteractive,
// stripped of everything that isn't "run the turn and hand back
// text": no spinner, no progress bar, no stdout writer. Those stay
// the caller's job (see cmd/run.go).
func (w *AppWorkspace) AgentRunStream(ctx context.Context, sessionID, prompt string) (<-chan AgentRunEvent, error) {
	coord := w.app.Coordinator()
	if coord == nil {
		return nil, errors.New("agent coordinator not initialized")
	}

	// Wait for MCP initialization to complete before reading MCP tools.
	if err := w.app.MCP.WaitForInit(ctx); err != nil {
		return nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	// Force-update agent models before running so MCP tools are loaded.
	if err := coord.UpdateModels(ctx); err != nil {
		return nil, fmt.Errorf("failed to update agent models: %w", err)
	}

	// Automatically approve all permission requests for this
	// non-interactive run.
	w.app.Permissions().AutoApproveSession(sessionID)

	// Report session identity to herdr. Local mode's Messages/RunComplete
	// event bridge (see herdr.BridgeLocal in app.New) does the rest.
	w.app.ReportCurrentSession(sessionID)

	ctx, cancel := context.WithCancel(ctx)
	out := make(chan AgentRunEvent)

	type response struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan response, 1)

	// Subscribed before the run starts, not inside the reader goroutine
	// below: a subscription taken after Run is already going misses
	// whatever it published in between, which for a short answer can be
	// the whole thing.
	messageEvents := w.app.Messages().Subscribe(ctx)

	go func() {
		result, err := coord.Run(ctx, sessionID, prompt)
		if err != nil {
			done <- response{err: fmt.Errorf("failed to start agent processing stream: %w", err)}
			return
		}
		done <- response{result: result}
	}()

	go func() {
		defer cancel()
		defer close(out)

		// send delivers ev unless the caller has gone away. It reports
		// whether the stream should keep going: a consumer that stopped
		// reading leaves this goroutine blocked on an unbuffered send
		// forever, and with it the deferred cancel and close, so every
		// send has to be able to give up.
		send := func(ev AgentRunEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		readBytes := make(map[string]int)
		var printed bool
		var lastStatus string

		// emit turns one message event into a text delta and/or a status
		// change, or reports that the stream must end — either because
		// the message content is corrupt (fail != nil) or because the
		// consumer went away mid-send (stop, with fail == nil). Shared by
		// the live loop and the drain after the run finishes, so both
		// produce identical output.
		emit := func(ev pubsub.Event[message.Message]) (stop bool, fail error) {
			msg := ev.Payload
			if msg.SessionID != sessionID || msg.Role != message.Assistant {
				return false, nil
			}
			// Status is derived before the empty-parts check on purpose:
			// the interesting status is exactly the one a message carries
			// before it has any content — request sent, nothing back yet,
			// or a tool call whose arguments are still streaming. A caller
			// showing a spinner would otherwise have nothing to say during
			// the longest silence of the turn.
			if status := msg.Working().Label(); status != "" && status != lastStatus {
				lastStatus = status
				if !send(AgentRunEvent{Status: status}) {
					return true, nil
				}
			}
			if len(msg.Parts) == 0 {
				return false, nil
			}
			content := msg.Content().String()
			rb := readBytes[msg.ID]
			if len(content) < rb {
				return true, fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), rb)
			}
			part := content[rb:]
			// Trim leading whitespace. Sometimes the LLM includes
			// leading formatting and indentation, which we don't
			// want here.
			if rb == 0 {
				part = strings.TrimLeft(part, " \t")
			}
			readBytes[msg.ID] = len(content)

			// Ignore initial whitespace-only messages.
			if !printed && strings.TrimSpace(part) == "" {
				return false, nil
			}
			printed = true
			if !send(AgentRunEvent{TextDelta: part}) {
				return true, nil
			}
			return false, nil
		}

		// drain empties whatever the run published just before it
		// returned. The last chunk of an answer is routinely still in the
		// subscription when done fires, and returning straight away
		// dropped it — the visible symptom being a reply that ends
		// mid-sentence.
		drain := func() error {
			for {
				select {
				case ev, ok := <-messageEvents:
					if !ok {
						return nil
					}
					if stop, err := emit(ev); stop {
						return err
					}
				default:
					return nil
				}
			}
		}

		for {
			select {
			case result := <-done:
				if result.err != nil {
					if errors.Is(result.err, context.Canceled) {
						send(AgentRunEvent{Done: true})
						return
					}
					send(AgentRunEvent{Done: true, Err: fmt.Errorf("agent processing failed: %w", result.err)})
					return
				}
				if err := drain(); err != nil {
					send(AgentRunEvent{Done: true, Err: err})
					return
				}
				send(AgentRunEvent{Done: true})
				return

			case ev, ok := <-messageEvents:
				if !ok {
					// The broker closed the channel (ctx was already
					// canceled). Stop selecting on it so the loop doesn't
					// spin on a permanently-ready closed channel; ctx.Done
					// below will fire and end the goroutine.
					messageEvents = nil
					continue
				}
				if stop, err := emit(ev); stop {
					if err != nil {
						send(AgentRunEvent{Done: true, Err: err})
					}
					return
				}

			case <-ctx.Done():
				send(AgentRunEvent{Done: true, Err: ctx.Err()})
				return
			}
		}
	}()

	return out, nil
}
