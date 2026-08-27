package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/notify"
	mcptools "github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/app/threadspawn"
	"github.com/rave-soft/sennit/internal/commands"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/thread"
)

// runAndCaptureStream and runAndPersist are indirected through package
// vars, rather than called directly on shell.*, so a test can substitute
// a failing stand-in without needing a shell command that actually fails
// to start (RunAndCaptureStream folds every real failure into
// CaptureResult.ExitCode and never itself returns a non-nil error).
var (
	runAndCaptureStream = shell.RunAndCaptureStream
	runAndPersist       = shell.RunAndPersist
)

// AppWorkspace implements the Workspace interface by delegating
// directly to an in-process [app.App] instance.
type AppWorkspace struct {
	app   *app.App
	store *config.ConfigStore
}

// NewAppWorkspace creates a new AppWorkspace wrapping the given app
// and config store.
func NewAppWorkspace(a *app.App, store *config.ConfigStore) *AppWorkspace {
	return &AppWorkspace{
		app:   a,
		store: store,
	}
}

// -- Sessions --

func (w *AppWorkspace) BackgroundJobCounts() shell.BackgroundJobCounts {
	return w.app.BackgroundShells.Counts()
}

func (w *AppWorkspace) CreateSession(ctx context.Context, title string) (session.Session, error) {
	return w.app.Sessions().Create(ctx, title)
}

func (w *AppWorkspace) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	return w.app.Sessions().Get(ctx, sessionID)
}

func (w *AppWorkspace) ListSessions(ctx context.Context) ([]session.Session, error) {
	return w.app.Sessions().List(ctx)
}

func (w *AppWorkspace) GetLastSession(ctx context.Context) (session.Session, error) {
	return w.app.Sessions().GetLast(ctx)
}

func (w *AppWorkspace) SaveSession(ctx context.Context, sess session.Session) (session.Session, error) {
	return w.app.Sessions().Save(ctx, sess)
}

func (w *AppWorkspace) DeleteSession(ctx context.Context, sessionID string) error {
	return w.app.Sessions().Delete(ctx, sessionID)
}

func (w *AppWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return w.app.Sessions().CreateAgentToolSessionID(messageID, toolCallID)
}

func (w *AppWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	return w.app.Sessions().ParseAgentToolSessionID(sessionID)
}

// SetCurrentSession reports the active session to herdr so the pane
// can persist a resumable reference. Multi-client presence tracking
// is irrelevant in single-client local mode, but herdr still needs
// to know which session is live to support agent resume.
func (w *AppWorkspace) SetCurrentSession(ctx context.Context, sessionID string) error {
	return w.SetCurrentSessionGeneration(ctx, sessionID, 0)
}

func (w *AppWorkspace) SetCurrentSessionGeneration(_ context.Context, sessionID string, _ uint64) error {
	w.app.ReportCurrentSession(sessionID)
	return nil
}

// -- Messages --

func (w *AppWorkspace) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	// Drain any debounced updates so the caller observes the latest
	// in-memory state. message.Service buffers streaming deltas and a
	// cold List would otherwise miss them at session-switch time.
	if err := w.app.Messages().FlushAll(ctx); err != nil {
		return nil, err
	}
	return w.app.Messages().List(ctx, sessionID)
}

func (w *AppWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	return w.app.Messages().ListUserMessages(ctx, sessionID)
}

func (w *AppWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	return w.app.Messages().ListAllUserMessages(ctx)
}

func (w *AppWorkspace) ListMessagesBySessionIDs(ctx context.Context, rootSessionID string, _ uint64, sessionIDs []string) (map[string][]message.Message, error) {
	validated, err := w.app.Sessions().ValidateSessionIDsInTree(ctx, rootSessionID, sessionIDs)
	if err != nil {
		return nil, err
	}
	if err := w.app.Messages().FlushAll(ctx); err != nil {
		return nil, err
	}
	return w.app.Messages().ListBySessionIDs(ctx, validated)
}

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

// -- Permissions --

// permissionsFor resolves the service actually holding perm, which is not
// always this workspace's own: a thread's prompts are raised inside its
// isolated workspace and relayed here for display (see
// lifecycle.forwardPermissions), so the answer has to travel back to the
// service that is still blocking on it. Falls back to this workspace's own
// service for everything else — the user's own turn, and tasks, which run
// in this very App.
func (w *AppWorkspace) permissionsFor(perm permission.PermissionRequest) []permission.Service {
	own := w.app.Permissions()
	if perm.Delegation.ID == "" {
		return []permission.Service{own}
	}
	mgr, ok := w.threadManager()
	if !ok {
		return []permission.Service{own}
	}
	if svc := mgr.PermissionsFor(perm.Delegation.ID); svc != nil {
		return []permission.Service{svc, own}
	}
	return []permission.Service{own}
}

// answerPermission hands perm to each candidate service in turn until one
// accepts it.
//
// Routing has to guess, and a wrong guess used to be fatal to the prompt.
// The tag a request carries is the delegation whose run raised it, which
// is not the same question as "which permission service is blocked on
// this id": a thread's runtime can have been replaced since the prompt was
// published, and the screen the answer is given on is not necessarily the
// workspace the prompt came from -- while the user is drilled into a
// thread, every event is routed to that thread's UI, including prompts
// raised by the parent workspace behind it. Answering the wrong service
// leaves the right one blocked forever with its dialog still on screen,
// and every further click reports "permission response was not accepted".
//
// Trying the others is safe rather than merely convenient: a service
// resolves a request only if it wins the take of that id from its own
// pending map (see permission.resolve), so a service that is not holding
// the request does nothing at all and says so. Order still matters --
// the routed service is asked first -- but only for cost, not
// correctness.
func answerPermission(attempts ...func() bool) bool {
	for _, attempt := range attempts {
		if attempt != nil && attempt() {
			return true
		}
	}
	return false
}

// serviceAttempts adapts candidate services into answerPermission attempts.
func serviceAttempts(services []permission.Service, answer func(permission.Service) bool) []func() bool {
	attempts := make([]func() bool, 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		attempts = append(attempts, func() bool { return answer(svc) })
	}
	return attempts
}

func (w *AppWorkspace) PermissionGrant(perm permission.PermissionRequest) bool {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Service) bool { return s.Grant(perm) })...)
}

func (w *AppWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) bool {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Service) bool { return s.GrantPersistent(perm) })...)
}

func (w *AppWorkspace) PermissionDeny(perm permission.PermissionRequest) bool {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Service) bool { return s.Deny(perm) })...)
}

func (w *AppWorkspace) PermissionSkipRequests() bool {
	return w.app.Permissions().SkipRequests()
}

func (w *AppWorkspace) PermissionSetSkipRequests(skip bool) {
	w.app.SetPermissionsSkip(skip)
}

// -- Questions --

func (w *AppWorkspace) QuestionAnswer(responses []question.Answer) bool {
	return w.app.Questions.Answer(responses)
}

func (w *AppWorkspace) QuestionCancel() bool {
	return w.app.Questions.Cancel()
}

// -- FileTracker --

func (w *AppWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]SessionFile, error) {
	return prepareSessionChanges(ctx, sessionID, w.ListSessionHistory, w.UncommittedFiles)
}

func prepareSessionChanges(
	ctx context.Context,
	sessionID string,
	listHistory func(context.Context, string) ([]history.File, error),
	uncommittedFiles func(context.Context) ([]git.FileChange, error),
) ([]SessionFile, error) {
	historyFiles, err := listHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := AggregateSessionFiles(historyFiles)
	uncommitted, err := uncommittedFiles(ctx)
	if err != nil {
		slog.Warn("Failed to load uncommitted files for session", "session_id", sessionID, "error", err)
		return files, nil
	}
	return MarkUncommittedSessionFiles(files, uncommitted), nil
}

func (w *AppWorkspace) UncommittedFiles(ctx context.Context) ([]git.FileChange, error) {
	return git.UncommittedFiles(ctx, w.store.WorkingDir())
}

func (w *AppWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {
	w.app.FileTracker.RecordRead(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return w.app.FileTracker.LastReadTime(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return w.app.FileTracker.ListReadFiles(ctx, sessionID)
}

// -- History --

func (w *AppWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	return w.app.History.ListBySessionTree(ctx, sessionID)
}

// -- LSP --

func (w *AppWorkspace) LSPStart(ctx context.Context, path string) {
	w.app.LSPManager.Start(ctx, path)
}

func (w *AppWorkspace) LSPStopAll(ctx context.Context) {
	w.app.LSPManager.StopAll(ctx)
}

func (w *AppWorkspace) LSPGetStates() map[string]LSPClientInfo {
	return w.app.GetLSPStates()
}

func (w *AppWorkspace) LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts {
	state, ok := w.app.GetLSPState(name)
	if !ok || state.Client == nil {
		return lsp.DiagnosticCounts{}
	}
	return state.Client.GetDiagnosticCounts()
}

// -- Config (read-only) --

func (w *AppWorkspace) Config() *config.Config {
	return w.store.Config()
}

func (w *AppWorkspace) WorkingDir() string {
	return w.store.WorkingDir()
}

func (w *AppWorkspace) Resolver() config.VariableResolver {
	return w.store.Resolver()
}

// -- Config mutations --

func (w *AppWorkspace) UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error {
	return w.store.UpdatePreferredModel(scope, model)
}

// OverridePreferredModel sets the model in memory only, without
// touching the user's config file. See the Workspace interface doc.
func (w *AppWorkspace) OverridePreferredModel(model config.SelectedModel) error {
	w.store.OverridePreferredModel(model)
	return nil
}

func (w *AppWorkspace) SetCompactMode(scope config.Scope, enabled bool) error {
	return w.store.SetCompactMode(scope, enabled)
}

func (w *AppWorkspace) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	if err := w.store.SetProviderAPIKey(scope, providerID, apiKey); err != nil {
		return err
	}
	w.app.Credentials().SignalAuthComplete(providerID)
	return nil
}

// RecordAccount implements Workspace by delegating to config.RecordAccount.
// The account store is opened fresh on each call rather than cached on
// AppWorkspace: FileStore is a thin, stateless wrapper over the file path
// (see its doc comment — it holds no in-memory cache), so there is nothing
// worth keeping alive between calls.
func (w *AppWorkspace) RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	a, err := config.RecordAccount(w.store, accStore, scope, providerID, cred)
	if err != nil {
		return accounts.Account{}, err
	}
	w.app.Credentials().SignalAuthComplete(providerID)
	return a, nil
}

// ListAccounts implements Workspace by delegating to the account store.
// See RecordAccount's comment for why the store is opened fresh here
// rather than cached on AppWorkspace. It first folds any pre-existing
// single credential into the store (config.EnsureAccountMigrated), so a
// user who authenticated before the multi-account feature existed sees
// their own account here instead of an empty list.
func (w *AppWorkspace) ListAccounts(providerID string) ([]accounts.Account, error) {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	if err := config.EnsureAccountMigrated(w.store, accStore, providerID); err != nil {
		return nil, err
	}
	return accStore.List(providerID)
}

// ActivateAccount implements Workspace by looking up accountID and
// delegating the switch to config.ConfigStore.ActivateAccount.
func (w *AppWorkspace) ActivateAccount(scope config.Scope, providerID, accountID string) error {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	a, ok, err := accStore.Get(providerID, accountID)
	if err != nil {
		return fmt.Errorf("looking up account %s for provider %s: %w", accountID, providerID, err)
	}
	if !ok {
		return fmt.Errorf("account %s not found for provider %s", accountID, providerID)
	}
	return w.store.ActivateAccount(scope, providerID, a)
}

// UpdateAccount implements Workspace by delegating to config.UpdateAccount.
// The account store is opened fresh here rather than cached on
// AppWorkspace — see RecordAccount's comment above for why. The rules
// themselves (upsert, then republish if active) live in config.UpdateAccount
// so this stays a thin wrapper: see that function's doc comment for why a
// second copy of the logic here would be the wrong shape.
func (w *AppWorkspace) UpdateAccount(providerID string, account accounts.Account) error {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	return config.UpdateAccount(w.store, accStore, providerID, account)
}

// RemoveAccount implements Workspace by delegating to config.RemoveAccount,
// which owns the actual rules (refuse the last account, activate a
// replacement before deleting the active one) — see its doc comment for
// why those rules must not be duplicated here or in a test double.
func (w *AppWorkspace) RemoveAccount(scope config.Scope, providerID, accountID string) error {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	return config.RemoveAccount(w.store, accStore, scope, providerID, accountID)
}

// SetProviderProxy implements Workspace by delegating to
// config.SetProviderProxy, exactly like UpdateAccount/RemoveAccount above.
func (w *AppWorkspace) SetProviderProxy(providerID, proxy string) error {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	return config.SetProviderProxy(w.store, accStore, providerID, proxy)
}

// RefreshAccountLimits implements Workspace by delegating to
// config.RefreshAccountLimits, exactly like UpdateAccount/RemoveAccount
// above.
func (w *AppWorkspace) RefreshAccountLimits(ctx context.Context, providerID string) ([]accounts.Account, error) {
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	return config.RefreshAccountLimits(ctx, w.store, accStore, providerID)
}

func (w *AppWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	return w.store.SetConfigField(scope, key, value)
}

func (w *AppWorkspace) RemoveConfigField(scope config.Scope, key string) error {
	return w.store.RemoveConfigField(scope, key)
}

func (w *AppWorkspace) ImportCopilot() (*oauth.Token, bool) {
	return w.app.Credentials().ImportCopilot()
}

func (w *AppWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	return w.app.Credentials().RefreshOAuthToken(ctx, scope, providerID)
}

// -- Project lifecycle --

func (w *AppWorkspace) ProjectNeedsInitialization() (bool, error) {
	return config.ProjectNeedsInitialization(w.store)
}

func (w *AppWorkspace) MarkProjectInitialized() error {
	return config.MarkProjectInitialized(w.store)
}

func (w *AppWorkspace) InitializePrompt() (string, error) {
	return agent.InitializePrompt(w.store)
}

func (w *AppWorkspace) ListSkills(_ context.Context) ([]skills.CatalogEntry, error) {
	mgr := w.app.Skills
	return skills.Catalog(mgr.ActiveSkills(), mgr.ResolvedPaths(), mgr.WorkingDir()), nil
}

func (w *AppWorkspace) ReadSkill(_ context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	mgr := w.app.Skills
	return skills.ReadContent(mgr.ActiveSkills(), mgr.ResolvedPaths(), mgr.WorkingDir(), skillID)
}

// -- MCP operations --

func (w *AppWorkspace) WaitForMCPInit(ctx context.Context) error {
	return w.app.MCP.WaitForInit(ctx)
}

// Stats implements [UsageReporter] by forwarding to the App, which owns
// the database connection the aggregation reads.
func (w *AppWorkspace) Stats(ctx context.Context, req stats.Request) (stats.Snapshot, error) {
	return w.app.Stats(ctx, req)
}

func (w *AppWorkspace) MCPGetStates() map[string]MCPClientInfo {
	states := w.app.MCP.GetStates()
	result := make(map[string]MCPClientInfo, len(states))
	for name, state := range states {
		result[name] = MCPClientInfo{Name: state.Name, State: MCPState(state.State), Error: state.Error, Counts: MCPCounts{Tools: state.Counts.Tools, Prompts: state.Counts.Prompts, Resources: state.Counts.Resources}, ConnectedAt: state.ConnectedAt}
	}
	return result
}

func (w *AppWorkspace) MCPResources() []MCPResourceInfo {
	var result []MCPResourceInfo
	for mcpName, resources := range w.app.MCP.Resources() {
		for _, r := range resources {
			result = append(result, MCPResourceInfo{
				MCPName:  mcpName,
				URI:      r.URI,
				Title:    r.Name,
				MIMEType: r.MIMEType,
			})
		}
	}
	return result
}

func (w *AppWorkspace) MCPRefreshPrompts(ctx context.Context, name string) {
	w.app.MCP.RefreshPrompts(ctx, name)
}

func (w *AppWorkspace) MCPRefreshResources(ctx context.Context, name string) {
	w.app.MCP.RefreshResources(ctx, name)
}

func (w *AppWorkspace) RefreshMCPTools(ctx context.Context, name string) {
	w.app.MCP.RefreshTools(ctx, w.store, name)
}

func (w *AppWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error) {
	contents, err := w.app.MCP.ReadResource(ctx, w.store, name, uri)
	if err != nil {
		return nil, err
	}
	result := make([]MCPResourceContents, len(contents))
	for i, c := range contents {
		result[i] = MCPResourceContents{
			URI:      c.URI,
			MIMEType: c.MIMEType,
			Text:     c.Text,
			Blob:     c.Blob,
		}
	}
	return result, nil
}

func (w *AppWorkspace) ListMCPPrompts(context.Context) ([]commands.MCPPrompt, error) {
	return commands.LoadMCPPrompts(w.app.MCP)
}

func (w *AppWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return commands.GetMCPPrompt(w.app.MCP, w.store, clientID, promptID, args)
}

func (w *AppWorkspace) EnableDockerMCP(ctx context.Context) error {
	mcpConfig, err := w.store.PrepareDockerMCPConfig()
	if err != nil {
		return err
	}

	if err := w.app.MCP.InitializeSingle(ctx, config.DockerMCPName, w.store); err != nil {
		disableErr := w.app.MCP.DisableSingle(w.store, config.DockerMCPName)
		w.store.RemoveDockerMCPInMemory()
		return fmt.Errorf("failed to start docker MCP: %w", errors.Join(err, disableErr))
	}

	if err := w.store.PersistDockerMCPConfig(mcpConfig); err != nil {
		disableErr := w.app.MCP.DisableSingle(w.store, config.DockerMCPName)
		w.store.RemoveDockerMCPInMemory()
		return fmt.Errorf("docker MCP started but failed to persist configuration: %w", errors.Join(err, disableErr))
	}

	return nil
}

func (w *AppWorkspace) DisableDockerMCP() error {
	if err := w.app.MCP.DisableSingle(w.store, config.DockerMCPName); err != nil {
		return fmt.Errorf("failed to disable docker MCP: %w", err)
	}
	return w.store.DisableDockerMCP()
}

func (w *AppWorkspace) MCPAuthenticate(ctx context.Context, name string) error {
	return w.app.MCP.AuthenticateMCP(ctx, w.store, name)
}

func (w *AppWorkspace) MCPPendingAuth() []MCPPendingAuthServer {
	pending := w.app.MCP.PendingAuthMCPs(w.store)
	result := make([]MCPPendingAuthServer, len(pending))
	for i, server := range pending {
		result[i] = MCPPendingAuthServer{Name: server.Name, URL: server.URL}
	}
	return result
}

func (w *AppWorkspace) MCPAuthURL(name string) string {
	return w.app.MCP.MCPAuthURL(name)
}

// -- Lifecycle --

func (w *AppWorkspace) Subscribe(send func(any)) {
	w.app.Subscribe(func(msg any) { send(w.translateEvent(msg)) }, w.app.Shutdown)
}

// translateEvent adapts a message from app's event fan-in into the shape
// the TUI's Update() expects. Every source app.setupEvents wires in at
// construction already arrives pre-shaped; the one exception is thread
// events, forwarded raw by app.ForwardEvents (see
// internal/app/threadspawn/attach.go) as the
// pubsub.Event[thread.Event] the Manager itself publishes, because
// ForwardEvents is generic over T and has no way to convert on the way
// in. Convert here, at the UI-facing boundary, into
// pubsub.Event[proto.Thread] so threads_dock.go, thread_indicator.go,
// thread_completion.go and threads.go (the dashboard) see live updates
// instead of relying solely on their TTL-poll fallback. Any other
// message passes through unchanged.
func (w *AppWorkspace) translateEvent(msg any) any {
	switch e := msg.(type) {
	case pubsub.Event[notify.Notification]:
		return pubsub.Event[AgentNotification]{Type: e.Type, Payload: AgentNotification{SessionID: e.Payload.SessionID, SessionTitle: e.Payload.SessionTitle, Type: AgentNotificationType(e.Payload.Type), ProviderID: e.Payload.ProviderID, RunID: e.Payload.RunID, Message: e.Payload.Message, AWSSOCommand: e.Payload.AWSSOCommand, AWSSOURL: e.Payload.AWSSOURL}}
	case pubsub.Event[mcptools.Event]:
		var eventType MCPEventType
		switch e.Payload.Type {
		case mcptools.EventStateChanged:
			eventType = MCPEventStateChanged
		case mcptools.EventToolsListChanged:
			eventType = MCPEventToolsListChanged
		case mcptools.EventPromptsListChanged:
			eventType = MCPEventPromptsListChanged
		case mcptools.EventResourcesListChanged:
			eventType = MCPEventResourcesListChanged
		default:
			return nil
		}
		return pubsub.Event[MCPEvent]{Type: e.Type, Payload: MCPEvent{Type: eventType, Name: e.Payload.Name}}
	case pubsub.Event[app.LSPEvent]:
		return pubsub.Event[LSPEvent]{Type: e.Type, Payload: LSPEvent{Type: LSPEventType(e.Payload.Type), Name: e.Payload.Name, State: e.Payload.State, Error: e.Payload.Error, DiagnosticCount: e.Payload.DiagnosticCount}}
	case app.UpdateAvailableMsg:
		return UpdateAvailableMsg{CurrentVersion: e.CurrentVersion, LatestVersion: e.LatestVersion, IsDevelopment: e.IsDevelopment}
	}
	e, ok := msg.(pubsub.Event[thread.Event])
	if !ok {
		return msg
	}
	// The manager (still attached — it's what published this event) can
	// resolve the thread's live WorkspaceID.
	workspaceID := ""
	if mgr, ok := w.threadManager(); ok {
		workspaceID = mgr.WorkspaceID(e.Payload.Thread.ID)
	}
	pe := threadspawn.EventToProto(e.Payload, workspaceID)
	return pubsub.Event[proto.Thread]{
		Type:    threadEventPubsubType(pe.Type),
		Payload: pe.Thread,
	}
}

func (w *AppWorkspace) Shutdown() {
	w.app.Shutdown()
}

// App returns the underlying app.App instance.
func (w *AppWorkspace) App() *app.App {
	return w.app
}

// Store returns the underlying config store.
func (w *AppWorkspace) Store() *config.ConfigStore {
	return w.store
}

// Compile-time check that AppWorkspace implements Workspace.
var _ Workspace = (*AppWorkspace)(nil)
