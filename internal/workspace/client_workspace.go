package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/google/uuid"
	"github.com/pkg/browser"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/client"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/herdr"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/log"
	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/rave-soft/braid/internal/version"
)

// ClientWorkspace implements the Workspace interface by delegating all
// operations to a remote server via the client SDK. It caches the
// proto.Workspace returned at creation time and refreshes it after
// config-mutating operations.
type ClientWorkspace struct {
	client *client.Client

	mu     sync.RWMutex
	ws     proto.Workspace
	skills *skills.Manager
	// lastSession is the most recent session ID reported via
	// SetCurrentSession. The subscription loop re-asserts it after a
	// reconnect, because the server's per-client presence entry (or the
	// whole workspace) may have been re-created in the meantime.
	lastSession string

	supportsThreads          threadsSupport
	threadsProbeStarted      bool
	threadsProbeDone         chan struct{}
	threadsProbeComplete     bool
	threadsProbeErr          error
	threadsProbeGeneration   uint64
	threadsProbes            sync.WaitGroup
	threadsProbeAdmissionOff bool

	// subCtx bounds the lifetime of the event subscription (and its
	// reconnect loop). Shutdown cancels it so Subscribe stops
	// reconnecting instead of racing the teardown.
	subCtx    context.Context
	subCancel context.CancelFunc
	// subStarted reports whether the subscription loop ever ran, and
	// subDone is closed when it returns. Shutdown uses them to let an
	// in-flight workspace recovery finish before it says goodbye to the
	// server, so the workspace it releases is the one recovery just
	// minted.
	subStarted atomic.Bool
	subDone    chan struct{}

	// herdrClient reports agent state to herdr when running inside
	// a herdr-managed pane. Nil when not in a herdr environment.
	herdrClient *herdr.Client
}

// SSE reconnect backoff bounds for the workspace event stream. Declared
// as vars (not consts) so tests can shrink the delays.
var (
	sseReconnectInitialBackoff = 250 * time.Millisecond
	sseReconnectMaxBackoff     = 10 * time.Second
)

// NewClientWorkspace creates a new ClientWorkspace that proxies all
// operations through the given client SDK. The ws parameter is the
// proto.Workspace snapshot returned by the server at creation time. The
// snapshot's Skills field seeds a process-local skills.Manager so the
// TUI sees discovery state before the first SSE event arrives. The
// manager is constructed with WithGlobalMirror because the client
// process represents exactly one workspace and the TUI reads
// skills.GetLatestStates directly at construction time.
func NewClientWorkspace(c *client.Client, ws proto.Workspace) *ClientWorkspace {
	if ws.Config != nil {
		ws.Config.SetupAgents()
	}
	states := protoToSkillStates(ws.Skills)
	mgr := skills.NewManager(nil, nil, states, skills.WithGlobalMirror())
	subCtx, subCancel := context.WithCancel(context.Background())

	ws_ := &ClientWorkspace{
		client:      c,
		ws:          ws,
		skills:      mgr,
		subCtx:      subCtx,
		subCancel:   subCancel,
		subDone:     make(chan struct{}),
		herdrClient: herdr.Init(),
	}

	ws_.setThreadsSupported(ws.ThreadsSupported)

	return ws_
}

// refreshWorkspace re-fetches the workspace from the server, updating
// the cached snapshot and the SupportsThreads cache. Called after
// config-mutating operations.
func (w *ClientWorkspace) refreshWorkspace() {
	updated, err := w.client.GetWorkspace(context.Background(), w.workspaceID())
	if err != nil {
		slog.Error("Failed to refresh workspace", "error", err)
		return
	}
	if updated.Config != nil {
		updated.Config.SetupAgents()
	}
	w.mu.Lock()
	w.ws = *updated
	w.threadsProbeGeneration++
	w.threadsProbeComplete = false
	w.threadsProbeErr = nil
	w.setThreadsSupported(updated.ThreadsSupported)
	w.mu.Unlock()
	w.startThreadsProbe()
}

// cached returns a snapshot of the cached workspace.
func (w *ClientWorkspace) cached() proto.Workspace {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.ws
}

// workspaceID returns the cached workspace ID.
func (w *ClientWorkspace) workspaceID() string {
	return w.cached().ID
}

// -- Sessions --

func (w *ClientWorkspace) CreateSession(ctx context.Context, title string) (session.Session, error) {
	sess, err := w.client.CreateSession(ctx, w.workspaceID(), title)
	if err != nil {
		return session.Session{}, err
	}
	return protoToSession(*sess), nil
}

func (w *ClientWorkspace) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	sess, err := w.client.GetSession(ctx, w.workspaceID(), sessionID)
	if err != nil {
		return session.Session{}, err
	}
	return protoToSession(*sess), nil
}

func (w *ClientWorkspace) ListSessions(ctx context.Context) ([]session.Session, error) {
	protoSessions, err := w.client.ListSessions(ctx, w.workspaceID())
	if err != nil {
		return nil, err
	}
	sessions := make([]session.Session, len(protoSessions))
	for i, s := range protoSessions {
		sessions[i] = protoToSession(s)
	}
	return sessions, nil
}

func (w *ClientWorkspace) SaveSession(ctx context.Context, sess session.Session) (session.Session, error) {
	saved, err := w.client.SaveSession(ctx, w.workspaceID(), sessionToProto(sess))
	if err != nil {
		return session.Session{}, err
	}
	return protoToSession(*saved), nil
}

func (w *ClientWorkspace) DeleteSession(ctx context.Context, sessionID string) error {
	return w.client.DeleteSession(ctx, w.workspaceID(), sessionID)
}

func (w *ClientWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

func (w *ClientWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// SetCurrentSession reports the session this client is currently
// viewing to the server. Empty sessionID clears the entry. Errors
// are propagated to the caller; the TUI logs and ignores them since
// the presence record is a hint, not correctness-critical state.
func (w *ClientWorkspace) SetCurrentSession(ctx context.Context, sessionID string) error {
	w.herdrClient.SetSessionID(sessionID)
	w.mu.Lock()
	w.lastSession = sessionID
	w.mu.Unlock()
	return w.client.SetCurrentSession(ctx, w.workspaceID(), sessionID)
}

// -- Messages --

func (w *ClientWorkspace) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	msgs, err := w.client.ListMessages(ctx, w.workspaceID(), sessionID)
	if err != nil {
		return nil, err
	}
	return protoToMessages(msgs), nil
}

func (w *ClientWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	msgs, err := w.client.ListUserMessages(ctx, w.workspaceID(), sessionID)
	if err != nil {
		return nil, err
	}
	return protoToMessages(msgs), nil
}

func (w *ClientWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	msgs, err := w.client.ListAllUserMessages(ctx, w.workspaceID())
	if err != nil {
		return nil, err
	}
	return protoToMessages(msgs), nil
}

// -- Agent --

func (w *ClientWorkspace) AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	// The interactive TUI does not consume notify.RunComplete for
	// completion detection (it observes message events directly),
	// so passing an empty RunID is correct here: it skips the
	// correlator stamping path without functional consequences.
	return w.client.SendMessage(ctx, w.workspaceID(), sessionID, "", prompt, attachments...)
}

func (w *ClientWorkspace) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, _ func(string), _ bool) (proto.ShellCommandResponse, error) {
	return w.client.RunShellCommand(ctx, w.workspaceID(), sessionID, command, termWidth)
}

func (w *ClientWorkspace) AgentCancel(sessionID string) {
	_ = w.client.CancelAgentSession(context.Background(), w.workspaceID(), sessionID)
}

func (w *ClientWorkspace) AgentIsBusy() bool {
	info, err := w.client.GetAgentInfo(context.Background(), w.workspaceID())
	if err != nil {
		return false
	}
	return info.IsBusy
}

func (w *ClientWorkspace) AgentIsSessionBusy(sessionID string) bool {
	info, err := w.client.GetAgentSessionInfo(context.Background(), w.workspaceID(), sessionID)
	if err != nil {
		return false
	}
	return info.IsBusy
}

func (w *ClientWorkspace) AgentModel() AgentModel {
	info, err := w.client.GetAgentInfo(context.Background(), w.workspaceID())
	if err != nil {
		return AgentModel{}
	}
	return AgentModel{
		CatalogCfg: info.Model,
		ModelCfg:   info.ModelCfg,
	}
}

func (w *ClientWorkspace) AgentIsReady() bool {
	return w.AgentReadyErr() == nil
}

func (w *ClientWorkspace) AgentReadyErr() error {
	info, err := w.client.GetAgentInfo(context.Background(), w.workspaceID())
	if err != nil {
		if errors.Is(err, client.ErrNotFound) {
			// The server answered, it just does not know this workspace
			// any more. The subscription loop is already re-registering;
			// saying "lost connection" here would be plainly wrong.
			return ErrWorkspaceGone
		}
		// The workspace/server could not be reached. This is distinct
		// from an initialized-but-not-ready agent: the server may have
		// torn the workspace down or restarted underneath us.
		return fmt.Errorf("%w: %v", ErrServerUnreachable, err)
	}
	if !info.IsReady {
		return ErrAgentNotInitialized
	}
	return nil
}

func (w *ClientWorkspace) AgentQueuedPrompts(sessionID string) int {
	count, err := w.client.GetAgentSessionQueuedPrompts(context.Background(), w.workspaceID(), sessionID)
	if err != nil {
		return 0
	}
	return count
}

func (w *ClientWorkspace) AgentQueuedPromptsList(sessionID string) []string {
	prompts, err := w.client.GetAgentSessionQueuedPromptsList(context.Background(), w.workspaceID(), sessionID)
	if err != nil {
		return nil
	}
	return prompts
}

func (w *ClientWorkspace) AgentClearQueue(sessionID string) {
	_ = w.client.ClearAgentSessionQueuedPrompts(context.Background(), w.workspaceID(), sessionID)
}

func (w *ClientWorkspace) AgentSummarize(ctx context.Context, sessionID string) error {
	return w.client.AgentSummarizeSession(ctx, w.workspaceID(), sessionID)
}

func (w *ClientWorkspace) UpdateAgentModel(ctx context.Context) error {
	return w.client.UpdateAgent(ctx, w.workspaceID())
}

func (w *ClientWorkspace) InitCoderAgent(ctx context.Context) error {
	return w.client.InitiateAgentProcessing(ctx, w.workspaceID(), true)
}

func (w *ClientWorkspace) InitCoderAgentNonInteractive(ctx context.Context) error {
	return w.client.InitiateAgentProcessing(ctx, w.workspaceID(), false)
}

func (w *ClientWorkspace) GetDefaultSmallModel(providerID string) config.SelectedModel {
	model, err := w.client.GetDefaultSmallModel(context.Background(), w.workspaceID(), providerID)
	if err != nil {
		return config.SelectedModel{}
	}
	return *model
}

// AgentRunStream sends prompt as a new turn over the client SDK and
// streams the SSE event feed to completion. This is the run-loop body
// that used to live in cmd's runNonInteractive/runStream, moved here
// so cmd only ever talks to the Workspace interface.
//
// herdr integration for the client/server path lives here rather than
// in cmd: local mode already gets it transparently via
// herdr.BridgeLocal wired up inside app.New, so mirroring that inside
// ClientWorkspace (instead of leaking raw SSE events back to cmd for
// translation) keeps the two Workspace implementations symmetric.
func (w *ClientWorkspace) AgentRunStream(ctx context.Context, sessionID, prompt string) (<-chan AgentRunEvent, error) {
	wsID := w.workspaceID()

	if err := w.waitForAgent(ctx); err != nil {
		return nil, fmt.Errorf("agent not ready: %w", err)
	}

	// Force-update agent models so MCP tools are loaded.
	if err := w.client.UpdateAgent(ctx, wsID); err != nil {
		slog.Warn("Failed to update agent", "error", err)
	}

	events, err := w.client.SubscribeEvents(ctx, wsID)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to events: %w", err)
	}

	// Mint a per-call RunID so we can correlate the terminal
	// RunComplete with *this* SendMessage even if the session was busy
	// and another turn finished first. Without it the stream loop
	// would exit on whichever RunComplete arrived first for the same
	// session and drop the queued prompt's output.
	runID := uuid.New().String()
	if err := w.client.SendMessage(ctx, wsID, sessionID, runID, prompt); err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	// Start herdr integration for this run; released when the stream
	// loop exits, mirroring the pre-refactor one-shot `braid run`
	// process lifetime (the process exits shortly after this
	// returns).
	w.herdrClient.SetSessionID(sessionID)

	stream := &runStream{sessionID: sessionID, runID: runID}
	out := make(chan AgentRunEvent)

	go func() {
		defer close(out)
		defer w.herdrClient.Close()

		for {
			select {
			case ev, ok := <-events:
				if !ok {
					out <- AgentRunEvent{Done: true}
					return
				}

				// Forward events to herdr if running inside a herdr pane.
				if hev := herdr.Translate(ev); hev != nil {
					w.herdrClient.HandleEvent(hev)
				}

				delta, done, err := stream.handle(ev)
				if delta != "" {
					out <- AgentRunEvent{TextDelta: delta}
				}
				if done {
					out <- AgentRunEvent{Done: true, Err: err}
					return
				}

			case <-ctx.Done():
				out <- AgentRunEvent{Done: true, Err: ctx.Err()}
				return
			}
		}
	}()

	return out, nil
}

// waitForAgent polls GetAgentInfo until the agent is ready, with a
// timeout.
func (w *ClientWorkspace) waitForAgent(ctx context.Context) error {
	wsID := w.workspaceID()
	timeout := time.After(30 * time.Second)
	for {
		info, err := w.client.GetAgentInfo(ctx, wsID)
		if err == nil && info.IsReady {
			return nil
		}
		select {
		case <-timeout:
			if err != nil {
				return fmt.Errorf("timeout waiting for agent: %w", err)
			}
			return fmt.Errorf("timeout waiting for agent readiness")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// runStream tracks the per-message read cursor and the reconciliation
// state used by [ClientWorkspace.AgentRunStream] to translate
// streaming SSE events into a de-duplicated sequence of text deltas
// for `braid run`. It is split out so the state machine can be
// exercised in unit tests without spinning up the full server/client
// harness.
//
// runID, when non-empty, is the authoritative correlator for the
// terminal RunComplete event: the stream suppresses live message
// events and only exits on a RunComplete whose RunID matches, so a
// turn that finishes first on the same session (e.g. when our prompt
// was queued behind a busy session) cannot contaminate the output or
// terminate us prematurely. When empty (older servers, tests that
// don't supply one) the stream falls back to SessionID-only matching
// and live message streaming, which is still correct for the
// single-turn case.
type runStream struct {
	sessionID string
	runID     string
	read      map[string]int
	printed   bool
}

// handle processes one SSE event. Returns delta (new text to emit,
// already trimmed/de-duplicated), done=true when the run loop should
// exit (RunComplete observed), and a non-nil err only when the agent
// run failed (not on context cancel — that path is handled by the
// caller's select).
func (s *runStream) handle(ev any) (delta string, done bool, err error) {
	if s.read == nil {
		s.read = make(map[string]int)
	}
	switch e := ev.(type) {
	case pubsub.Event[proto.Message]:
		msg := e.Payload
		if msg.SessionID != s.sessionID || msg.Role != proto.Assistant || len(msg.Parts) == 0 {
			return "", false, nil
		}
		if s.runID != "" {
			return "", false, nil
		}

		content := msg.Content().String()
		readBytes := s.read[msg.ID]
		if len(content) < readBytes {
			slog.Error("Non-interactive: message content shorter than read bytes",
				"message_length", len(content), "read_bytes", readBytes)
			return "", false, fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
		}

		part := content[readBytes:]
		if readBytes == 0 {
			part = strings.TrimLeft(part, " \t")
		}
		s.read[msg.ID] = len(content)
		if !s.printed && strings.TrimSpace(part) == "" {
			return "", false, nil
		}
		s.printed = true
		return part, false, nil

	case pubsub.Event[proto.RunComplete]:
		// RunComplete is the authoritative end-of-run signal. We exit
		// on it instead of guessing from message finish parts, which
		// fire on every tool-call step too and were the source of the
		// regression where `braid run` exited mid-turn on
		// finish.reason == tool_use.
		//
		// Correlation:
		//   - if we minted a RunID for this SendMessage, only the
		//     event whose RunID matches is ours; any other turn
		//     finishing first on the same session (busy-session queue
		//     path) must be ignored.
		//   - if we have no RunID (older server, tests), fall back to
		//     SessionID matching.
		if s.runID != "" {
			if e.Payload.RunID != s.runID {
				return "", false, nil
			}
		} else if e.Payload.SessionID != s.sessionID {
			return "", false, nil
		}
		if e.Payload.Error != "" && !e.Payload.Cancelled {
			return "", true, fmt.Errorf("agent run failed: %s", e.Payload.Error)
		}
		// Reconcile against the authoritative final assistant text
		// carried in the event. The pubsub fan-in does not serialize
		// publishes across upstream brokers, so the final message
		// event may not have reached this loop yet; the embedded Text
		// field is the backstop that guarantees the full final text
		// always appears in the output.
		var tail string
		if e.Payload.MessageID != "" {
			full := e.Payload.Text
			readBytes := s.read[e.Payload.MessageID]
			if readBytes < len(full) {
				t := full[readBytes:]
				if readBytes == 0 {
					t = strings.TrimLeft(t, " \t")
				}
				if s.printed || strings.TrimSpace(t) != "" {
					s.printed = true
					tail = t
				}
			}
		}
		return tail, true, nil

	case pubsub.Event[proto.AgentEvent]:
		if e.Payload.Error == nil {
			return "", false, nil
		}
		// Attribute the error to our run before treating it as fatal.
		// Async errors from an unrelated workspace run share this
		// channel, so a foreign failure must not abort us:
		//   - if the event carries a RunID, it is the authoritative
		//     correlator: it must match our run exactly, otherwise it
		//     belongs to a different request and we ignore it.
		//   - if the event carries no RunID (older server), fall back
		//     to SessionID: it must be present and match our session,
		//     otherwise we ignore it.
		if e.Payload.RunID != "" {
			if e.Payload.RunID != s.runID {
				return "", false, nil
			}
		} else if e.Payload.SessionID == "" || e.Payload.SessionID != s.sessionID {
			return "", false, nil
		}
		return "", true, fmt.Errorf("agent error: %w", e.Payload.Error)
	}
	return "", false, nil
}

// -- Permissions --

func (w *ClientWorkspace) PermissionGrant(perm permission.PermissionRequest) bool {
	resolved, _ := w.client.GrantPermission(context.Background(), w.workspaceID(), proto.PermissionGrant{
		Permission: proto.PermissionRequest{
			ID:          perm.ID,
			SessionID:   perm.SessionID,
			ToolCallID:  perm.ToolCallID,
			ToolName:    perm.ToolName,
			Description: perm.Description,
			Action:      perm.Action,
			Path:        perm.Path,
			Params:      perm.Params,
		},
		Action: proto.PermissionAllow,
	})
	return resolved
}

func (w *ClientWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) bool {
	resolved, _ := w.client.GrantPermission(context.Background(), w.workspaceID(), proto.PermissionGrant{
		Permission: proto.PermissionRequest{
			ID:          perm.ID,
			SessionID:   perm.SessionID,
			ToolCallID:  perm.ToolCallID,
			ToolName:    perm.ToolName,
			Description: perm.Description,
			Action:      perm.Action,
			Path:        perm.Path,
			Params:      perm.Params,
		},
		Action: proto.PermissionAllowForSession,
	})
	return resolved
}

func (w *ClientWorkspace) PermissionDeny(perm permission.PermissionRequest) bool {
	resolved, _ := w.client.GrantPermission(context.Background(), w.workspaceID(), proto.PermissionGrant{
		Permission: proto.PermissionRequest{
			ID:          perm.ID,
			SessionID:   perm.SessionID,
			ToolCallID:  perm.ToolCallID,
			ToolName:    perm.ToolName,
			Description: perm.Description,
			Action:      perm.Action,
			Path:        perm.Path,
			Params:      perm.Params,
		},
		Action: proto.PermissionDeny,
	})
	return resolved
}

func (w *ClientWorkspace) PermissionSkipRequests() bool {
	skip, err := w.client.GetPermissionsSkipRequests(context.Background(), w.workspaceID())
	if err != nil {
		return false
	}
	return skip
}

func (w *ClientWorkspace) PermissionSetSkipRequests(skip bool) {
	_ = w.client.SetPermissionsSkipRequests(context.Background(), w.workspaceID(), skip)
}

// -- Questions --

// QuestionAnswer submits answers for a question via the client SDK.
func (w *ClientWorkspace) QuestionAnswer(responses []question.Answer) bool {
	protoResp := proto.QuestionAnswer{
		Responses: make([]proto.QuestionResponse, len(responses)),
	}
	for i, r := range responses {
		protoResp.Responses[i] = proto.QuestionResponse{
			QuestionID:  r.QuestionID,
			SelectedIDs: r.SelectedIDs,
			FillInText:  r.FillInText,
			Yes:         r.Yes,
			Notes:       r.Notes,
		}
	}
	resolved, err := w.client.AnswerQuestionBatch(context.Background(), w.workspaceID(), protoResp)
	if err != nil {
		slog.Error("Failed to answer question", "error", err)
		return false
	}
	return resolved
}

// QuestionCancel cancels the pending question via the client SDK.
func (w *ClientWorkspace) QuestionCancel() bool {
	cancelled, err := w.client.CancelQuestionBatch(context.Background(), w.workspaceID())
	if err != nil {
		slog.Error("Failed to cancel question", "error", err)
		return false
	}
	return cancelled
}

// -- FileTracker --

func (w *ClientWorkspace) UncommittedFiles(ctx context.Context) ([]git.FileChange, error) {
	return w.client.UncommittedFiles(ctx, w.workspaceID())
}

func (w *ClientWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {
	_ = w.client.FileTrackerRecordRead(ctx, w.workspaceID(), sessionID, path)
}

func (w *ClientWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	t, err := w.client.FileTrackerLastReadTime(ctx, w.workspaceID(), sessionID, path)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (w *ClientWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return w.client.FileTrackerListReadFiles(ctx, w.workspaceID(), sessionID)
}

// -- History --

func (w *ClientWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	files, err := w.client.ListSessionHistoryFiles(ctx, w.workspaceID(), sessionID)
	if err != nil {
		return nil, err
	}
	return protoToFiles(files), nil
}

// -- LSP --

func (w *ClientWorkspace) LSPStart(ctx context.Context, path string) {
	_ = w.client.LSPStart(ctx, w.workspaceID(), path)
}

func (w *ClientWorkspace) LSPStopAll(ctx context.Context) {
	_ = w.client.LSPStopAll(ctx, w.workspaceID())
}

func (w *ClientWorkspace) LSPGetStates() map[string]LSPClientInfo {
	states, err := w.client.GetLSPs(context.Background(), w.workspaceID())
	if err != nil {
		return nil
	}
	result := make(map[string]LSPClientInfo, len(states))
	for k, v := range states {
		result[k] = LSPClientInfo{
			Name:            v.Name,
			State:           v.State,
			Error:           v.Error,
			DiagnosticCount: v.DiagnosticCount,
			ConnectedAt:     v.ConnectedAt,
		}
	}
	return result
}

func (w *ClientWorkspace) LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts {
	diags, err := w.client.GetLSPDiagnostics(context.Background(), w.workspaceID(), name)
	if err != nil {
		return lsp.DiagnosticCounts{}
	}
	var counts lsp.DiagnosticCounts
	for _, fileDiags := range diags {
		for _, d := range fileDiags {
			switch d.Severity {
			case protocol.SeverityError:
				counts.Error++
			case protocol.SeverityWarning:
				counts.Warning++
			case protocol.SeverityInformation:
				counts.Information++
			case protocol.SeverityHint:
				counts.Hint++
			}
		}
	}
	return counts
}

// -- Config (read-only) --

func (w *ClientWorkspace) Config() *config.Config {
	return w.cached().Config
}

func (w *ClientWorkspace) WorkingDir() string {
	return w.cached().Path
}

func (w *ClientWorkspace) Resolver() config.VariableResolver {
	return config.IdentityResolver()
}

// -- Config mutations --

func (w *ClientWorkspace) UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error {
	err := w.client.UpdatePreferredModel(context.Background(), w.workspaceID(), scope, model)
	if err == nil {
		w.refreshWorkspace()
	}
	return err
}

// OverridePreferredModel has no server-side ephemeral equivalent, so
// it falls back to a persisted workspace-scoped update. See the
// Workspace interface doc.
func (w *ClientWorkspace) OverridePreferredModel(model config.SelectedModel) error {
	return w.UpdatePreferredModel(config.ScopeWorkspace, model)
}

func (w *ClientWorkspace) SetCompactMode(scope config.Scope, enabled bool) error {
	err := w.client.SetCompactMode(context.Background(), w.workspaceID(), scope, enabled)
	if err == nil {
		w.refreshWorkspace()
	}
	return err
}

func (w *ClientWorkspace) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	err := w.client.SetProviderAPIKey(context.Background(), w.workspaceID(), scope, providerID, apiKey)
	if err == nil {
		w.refreshWorkspace()
	}
	return err
}

func (w *ClientWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	err := w.client.SetConfigField(context.Background(), w.workspaceID(), scope, key, value)
	if err == nil {
		w.refreshWorkspace()
	}
	return err
}

func (w *ClientWorkspace) RemoveConfigField(scope config.Scope, key string) error {
	err := w.client.RemoveConfigField(context.Background(), w.workspaceID(), scope, key)
	if err == nil {
		w.refreshWorkspace()
	}
	return err
}

func (w *ClientWorkspace) ImportCopilot() (*oauth.Token, bool) {
	token, ok, err := w.client.ImportCopilot(context.Background(), w.workspaceID())
	if err != nil {
		return nil, false
	}
	if ok {
		w.refreshWorkspace()
	}
	return token, ok
}

func (w *ClientWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	err := w.client.RefreshOAuthToken(ctx, w.workspaceID(), scope, providerID)
	if err == nil {
		w.refreshWorkspace()
	}
	return err
}

// -- Project lifecycle --

func (w *ClientWorkspace) ProjectNeedsInitialization() (bool, error) {
	return w.client.ProjectNeedsInitialization(context.Background(), w.workspaceID())
}

func (w *ClientWorkspace) MarkProjectInitialized() error {
	return w.client.MarkProjectInitialized(context.Background(), w.workspaceID())
}

func (w *ClientWorkspace) InitializePrompt() (string, error) {
	return w.client.GetInitializePrompt(context.Background(), w.workspaceID())
}

func (w *ClientWorkspace) ListSkills(ctx context.Context) ([]skills.CatalogEntry, error) {
	entries, err := w.client.ListSkills(ctx, w.workspaceID())
	if err != nil {
		return nil, err
	}
	result := make([]skills.CatalogEntry, len(entries))
	for i, entry := range entries {
		result[i] = skills.CatalogEntry{
			ID:            entry.ID,
			Name:          entry.Name,
			Description:   entry.Description,
			Label:         entry.Label,
			Source:        skills.SourceType(entry.Source),
			UserInvocable: entry.UserInvocable,
		}
	}
	return result, nil
}

func (w *ClientWorkspace) ReadSkill(ctx context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	resp, err := w.client.ReadSkill(ctx, w.workspaceID(), skillID)
	if err != nil {
		return nil, skills.SkillReadResult{}, err
	}
	return resp.Content, skills.SkillReadResult{
		Name:        resp.Result.Name,
		Description: resp.Result.Description,
		Source:      skills.SourceType(resp.Result.Source),
		Builtin:     resp.Result.Builtin,
	}, nil
}

// -- MCP operations --

// WaitForMCPInit is a no-op for the remote client: MCP initialization runs
// server-side as part of workspace creation, and MCP state changes reach
// this client live over the event subscription, so there is nothing local
// to block on.
func (w *ClientWorkspace) WaitForMCPInit(context.Context) error {
	return nil
}

func (w *ClientWorkspace) MCPResources() []MCPResourceInfo {
	resources, err := w.client.ListMCPResources(context.Background(), w.workspaceID())
	if err != nil {
		slog.Warn("Failed to list MCP resources", "error", err)
		return nil
	}
	result := make([]MCPResourceInfo, len(resources))
	for i, r := range resources {
		result[i] = MCPResourceInfo{
			MCPName:  r.MCPName,
			URI:      r.URI,
			Title:    r.Title,
			MIMEType: r.MIMEType,
		}
	}
	return result
}

func (w *ClientWorkspace) MCPGetStates() map[string]mcp.ClientInfo {
	states, err := w.client.MCPGetStates(context.Background(), w.workspaceID())
	if err != nil {
		return nil
	}
	result := make(map[string]mcp.ClientInfo, len(states))
	for k, v := range states {
		result[k] = mcp.ClientInfo{
			Name:  v.Name,
			State: mcp.State(v.State),
			Error: v.Error,
			Counts: mcp.Counts{
				Tools:     v.ToolCount,
				Prompts:   v.PromptCount,
				Resources: v.ResourceCount,
			},
			ConnectedAt: v.ConnectedAt,
		}
	}
	return result
}

func (w *ClientWorkspace) MCPRefreshPrompts(ctx context.Context, name string) {
	_ = w.client.MCPRefreshPrompts(ctx, w.workspaceID(), name)
}

func (w *ClientWorkspace) MCPRefreshResources(ctx context.Context, name string) {
	_ = w.client.MCPRefreshResources(ctx, w.workspaceID(), name)
}

func (w *ClientWorkspace) RefreshMCPTools(ctx context.Context, name string) {
	_ = w.client.RefreshMCPTools(ctx, w.workspaceID(), name)
}

func (w *ClientWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error) {
	contents, err := w.client.ReadMCPResource(ctx, w.workspaceID(), name, uri)
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

func (w *ClientWorkspace) ListMCPPrompts(ctx context.Context) ([]commands.MCPPrompt, error) {
	prompts, err := w.client.ListMCPPrompts(ctx, w.workspaceID())
	if err != nil {
		return nil, err
	}
	result := make([]commands.MCPPrompt, len(prompts))
	for i, prompt := range prompts {
		arguments := make([]commands.Argument, len(prompt.Arguments))
		for j, argument := range prompt.Arguments {
			arguments[j] = commands.Argument{
				ID:          argument.ID,
				Title:       argument.Title,
				Description: argument.Description,
				Required:    argument.Required,
			}
		}
		result[i] = commands.MCPPrompt{
			ID:          prompt.ID,
			Title:       prompt.Title,
			Description: prompt.Description,
			PromptID:    prompt.PromptID,
			ClientID:    prompt.ClientID,
			Arguments:   arguments,
		}
	}
	return result, nil
}

func (w *ClientWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return w.client.GetMCPPrompt(context.Background(), w.workspaceID(), clientID, promptID, args)
}

func (w *ClientWorkspace) EnableDockerMCP(ctx context.Context) error {
	return w.client.EnableDockerMCP(ctx, w.workspaceID())
}

func (w *ClientWorkspace) DisableDockerMCP() error {
	return w.client.DisableDockerMCP(context.Background(), w.workspaceID())
}

func (w *ClientWorkspace) MCPAuthenticate(ctx context.Context, name string) error {
	// The server suppresses its own browser open for this flow; the client
	// polls the auth URL and opens it locally so the user authorizes on
	// their own machine. The OAuth callback listener runs on the server
	// (localhost ports shared when server and client are co-located).
	authErr := make(chan error, 1)
	go func() {
		authErr <- w.client.MCPAuthenticate(ctx, w.workspaceID(), name)
	}()

	// Poll for the authorization URL so we can open it in the local
	// browser as soon as the flow generates one.
	var opened bool
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-authErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if opened {
				continue
			}
			if u := w.MCPAuthURL(name); u != "" {
				if err := browser.OpenURL(u); err != nil {
					slog.Warn("Failed to open MCP OAuth URL in browser", "error", err)
				}
				opened = true
			}
		}
	}
}

func (w *ClientWorkspace) MCPPendingAuth() []mcp.PendingAuthServer {
	pending, err := w.client.MCPPendingAuth(context.Background(), w.workspaceID())
	if err != nil {
		slog.Warn("Failed to fetch MCP pending auth", "error", err)
		return nil
	}
	result := make([]mcp.PendingAuthServer, len(pending))
	for i, p := range pending {
		result[i] = mcp.PendingAuthServer{Name: p.Name, URL: p.URL}
	}
	return result
}

func (w *ClientWorkspace) MCPAuthURL(name string) string {
	// The server's in-progress authorization URL is exposed through the
	// pending-auth list while the flow runs; a server in StateNeedsAuth
	// paired with an active flow reports its URL here. Poll the server
	// for the in-flight URL.
	u, err := w.client.MCPAuthURL(context.Background(), w.workspaceID(), name)
	if err != nil {
		return ""
	}
	return u
}

// -- Lifecycle --

func (w *ClientWorkspace) Subscribe(program *tea.Program) {
	defer log.RecoverPanic("ClientWorkspace.Subscribe", func() {
		slog.Info("TUI subscription panic: attempting graceful shutdown")
		program.Quit()
	})

	w.runSubscription(program.Send)
}

// maxRecoveryEscalate is the number of consecutive failed workspace
// recovery attempts after which the loop tells the UI the connection
// looks unrecoverable. It keeps retrying regardless: a hard stop would
// thread a user whose server comes back a minute later, and Shutdown can
// always cancel it.
const maxRecoveryEscalate = 20

// recoveryCreateTimeout bounds a single re-registration attempt. It is
// generous because workspace startup is slow (config, database, LSP, MCP);
// it exists only so an unresponsive server cannot pin the subscription
// goroutine indefinitely, and the loop simply retries when it trips.
// A var, not a const, so tests can shrink it.
var recoveryCreateTimeout = 30 * time.Second

var threadsProbeTimeout = 5 * time.Second

// runSubscription subscribes to the workspace event stream and forwards
// translated events to send, reconnecting with capped exponential
// backoff whenever the stream drops. It returns only when the
// subscription context is cancelled (via Shutdown). Split out from
// Subscribe so it can be tested without a real *tea.Program.
//
// Two failures need more than a retry. A 404 means the server no longer
// knows this workspace, so resubscribing with the same ID can never
// succeed and the loop re-registers instead. And any stream that closes
// loses whatever was published while the client was away, so every
// re-established stream — even one that reconnects on the first try —
// re-asserts the client's session and asks the UI to resync.
func (w *ClientWorkspace) runSubscription(send func(tea.Msg)) {
	w.subStarted.Store(true)
	w.startThreadsProbe()
	defer close(w.subDone)

	backoff := sseReconnectInitialBackoff
	degraded := false
	recoveryFailures := 0
	markDegraded := func(err error, stuck bool) {
		if degraded && !stuck {
			return
		}
		degraded = true
		send(ConnectionEvent{State: ConnectionDegraded, Err: err, Stuck: stuck})
	}

	for {
		if w.subCtx.Err() != nil {
			return
		}

		evc, err := w.client.SubscribeEvents(w.subCtx, w.workspaceID())
		if err != nil {
			if w.subCtx.Err() != nil {
				return
			}
			markDegraded(err, false)
			if !errors.Is(err, client.ErrNotFound) {
				slog.Error("Failed to subscribe to workspace events; retrying",
					"error", err, "retry_in", backoff)
			} else if w.recoverWorkspace() == nil {
				// Re-registered: resubscribe immediately under the fresh
				// workspace ID.
				backoff = sseReconnectInitialBackoff
				continue
			} else if w.subCtx.Err() == nil {
				recoveryFailures++
				if recoveryFailures == maxRecoveryEscalate {
					markDegraded(ErrWorkspaceGone, true)
				}
			}
			if !w.sleepOrDone(backoff) {
				return
			}
			backoff = min(backoff*2, sseReconnectMaxBackoff)
			continue
		}

		if degraded {
			degraded = false
			recoveryFailures = 0
			w.afterReconnect(send)
		}
		backoff = sseReconnectInitialBackoff
		w.consumeEvents(evc, send)

		// The event channel closed: the server restarted, the stream was
		// interrupted, or the workspace briefly went away. Reconnect
		// after a short delay instead of leaving the TUI permanently
		// orphaned, which is what surfaced as a stuck "coder agent is
		// offline".
		if w.subCtx.Err() != nil {
			return
		}
		markDegraded(ErrStreamClosed, false)
		slog.Warn("Workspace event stream closed; reconnecting", "retry_in", backoff)
		if !w.sleepOrDone(backoff) {
			return
		}
		backoff = min(backoff*2, sseReconnectMaxBackoff)
	}
}

// recoverWorkspace re-registers the workspace after the server reported it
// gone: it re-creates it from the cached snapshot (the server's own view of
// path, data dir, flags and env), adopts the new ID, and re-initializes the
// coder agent when the config is ready, mirroring the startup handshake. The
// server's path dedupe means this either rejoins a live sibling workspace or
// mints a fresh one. It must only be called from the subscription goroutine,
// the only writer of the cached ID.
//
// The create deliberately runs detached from the subscription context: the
// server does not abandon a create when the requesting connection goes away,
// so cancelling would hide the outcome while the workspace got registered
// anyway. Riding it out means the ID is known by the time Shutdown looks,
// and retirement covers a lost response regardless. Its own timeout keeps a
// wedged server from pinning the subscription goroutine forever; the client
// SDK sets no request timeout of its own.
func (w *ClientWorkspace) recoverWorkspace() error {
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(w.subCtx), recoveryCreateTimeout,
	)
	defer cancel()
	created, err := w.client.CreateWorkspace(ctx, w.recreateArgs())
	if err != nil {
		slog.Error("Failed to re-register workspace; retrying", "error", err)
		return err
	}
	if created.Config != nil {
		created.Config.SetupAgents()
	}
	w.mu.Lock()
	oldID := w.ws.ID
	w.ws = *created
	w.threadsProbeGeneration++
	w.threadsProbeComplete = false
	w.threadsProbeErr = nil
	newID := created.ID
	w.setThreadsSupported(created.ThreadsSupported)
	w.mu.Unlock()
	w.startThreadsProbe()
	slog.Info("Re-registered workspace after server-side loss",
		"old_id", oldID, "new_id", newID)

	if created.Config != nil && created.Config.IsConfigured() {
		if err := w.InitCoderAgent(w.subCtx); err != nil {
			// Matches the startup handshake: agent init failure is
			// logged, not fatal, since the user can still pick a model.
			slog.Error("Failed to initialize coder agent after workspace recovery", "error", err)
		}
	}
	return nil
}

// recreateArgs derives the CreateWorkspace request used for recovery from
// the cached snapshot. The ID is dropped so the server can dedupe by
// path or mint a fresh workspace, and Version carries this client's
// version, matching the startup handshake.
func (w *ClientWorkspace) recreateArgs() proto.Workspace {
	ws := w.cached()
	return proto.Workspace{
		Path:     ws.Path,
		DataDir:  ws.DataDir,
		Debug:    ws.Debug,
		YOLO:     ws.YOLO,
		Channels: ws.Channels,
		Env:      ws.Env,
		Version:  version.Version,
	}
}

// afterReconnect runs once a degraded subscription is re-established. It
// re-asserts the client's current-session selection, since the server's
// presence entry (or the whole workspace) may have been re-created while we
// were away, and tells the UI to resync state published while detached. The
// SSE handler attaches the client before writing its 200, so the presence
// call cannot be rejected as not-attached here.
func (w *ClientWorkspace) afterReconnect(send func(tea.Msg)) {
	w.mu.RLock()
	sid := w.lastSession
	w.mu.RUnlock()
	if sid != "" {
		if err := w.SetCurrentSession(w.subCtx, sid); err != nil {
			slog.Warn("Failed to re-assert current session after reconnect", "error", err)
		}
	}
	send(ConnectionEvent{State: ConnectionRecovered})
}

// sleepOrDone waits for d or until the subscription context is
// cancelled. It reports false when the context was cancelled, signalling
// the caller to stop reconnecting.
func (w *ClientWorkspace) sleepOrDone(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-w.subCtx.Done():
		return false
	}
}

// consumeEvents drives the workspace event loop. It is split out from
// Subscribe so tests can drive it without a real *tea.Program.
// ConfigChanged events trigger a workspace refresh; all other events
// are translated into domain types and forwarded to send.
func (w *ClientWorkspace) consumeEvents(evc <-chan any, send func(tea.Msg)) {
	for ev := range evc {
		// Forward events to herdr if running inside a herdr pane.
		if hev := herdr.Translate(ev); hev != nil {
			w.herdrClient.HandleEvent(hev)
		}

		if _, ok := ev.(pubsub.Event[proto.ConfigChanged]); ok {
			w.refreshWorkspace()
			continue
		}
		translated := w.translateEvent(ev)
		if translated != nil && send != nil {
			send(translated)
		}
	}
}

// shutdownDrainTimeout bounds how long Shutdown waits for the subscription
// loop to stop. Exceeding it is not a correctness problem — retiring the
// client releases whatever a late recovery registers — it only makes the
// goodbye less tidy.
const shutdownDrainTimeout = 5 * time.Second

func (w *ClientWorkspace) Shutdown() {
	// Stop the reconnect/recovery loop first, then wait for it: cancelling
	// alone does not unwind a workspace recovery that is already in
	// flight, and we want to release the workspace that recovery ended up
	// with rather than one it is about to replace.
	if w.subCancel != nil {
		w.subCancel()
	}
	w.mu.Lock()
	w.threadsProbeAdmissionOff = true
	w.mu.Unlock()
	w.awaitSubscription()
	w.threadsProbes.Wait()
	w.herdrClient.Close()

	// Retiring the client releases every claim it holds, on every workspace,
	// and blocks any further create from this client ID. That is what makes
	// teardown exact even when a recovery create's response was lost: the
	// create either landed before this call, and its claim is released here,
	// or it arrives afterwards and registers nothing.
	err := w.client.RetireClient(context.Background())
	if err == nil {
		return
	}
	if !errors.Is(err, client.ErrUnsupported) {
		slog.Warn("Failed to retire client on the server", "error", err)
		return
	}
	// The server predates client retirement, so fall back to releasing
	// the workspace we know about. Nothing better is possible against an
	// older server.
	_ = w.client.DeleteWorkspace(context.Background(), w.workspaceID())
}

// awaitSubscription waits for the subscription loop to return. It returns
// immediately when the loop never started, which is the case for
// workspaces shut down before Subscribe runs.
func (w *ClientWorkspace) awaitSubscription() {
	if !w.subStarted.Load() || w.subDone == nil {
		return
	}
	t := time.NewTimer(shutdownDrainTimeout)
	defer t.Stop()
	select {
	case <-w.subDone:
	case <-t.C:
		slog.Warn("Timed out waiting for the workspace subscription to stop")
	}
}

// translateEvent converts proto-typed SSE events into the domain types
// that the TUI's Update() method expects. Skills events also update the
// process-local skills.Manager so callers reading
// skills.GetLatestStates see fresh data.
func (w *ClientWorkspace) translateEvent(ev any) tea.Msg {
	switch e := ev.(type) {
	case pubsub.Event[proto.LSPEvent]:
		return pubsub.Event[LSPEvent]{
			Type: e.Type,
			Payload: LSPEvent{
				Type:            LSPEventType(e.Payload.Type),
				Name:            e.Payload.Name,
				State:           e.Payload.State,
				Error:           e.Payload.Error,
				DiagnosticCount: e.Payload.DiagnosticCount,
			},
		}
	case pubsub.Event[proto.MCPEvent]:
		return pubsub.Event[mcp.Event]{
			Type: e.Type,
			Payload: mcp.Event{
				Type:  protoToMCPEventType(e.Payload.Type),
				Name:  e.Payload.Name,
				State: mcp.State(e.Payload.State),
				Error: e.Payload.Error,
				Counts: mcp.Counts{
					Tools:     e.Payload.ToolCount,
					Prompts:   e.Payload.PromptCount,
					Resources: e.Payload.ResourceCount,
				},
			},
		}
	case pubsub.Event[proto.PermissionRequest]:
		return pubsub.Event[permission.PermissionRequest]{
			Type: e.Type,
			Payload: permission.PermissionRequest{
				ID:          e.Payload.ID,
				SessionID:   e.Payload.SessionID,
				ToolCallID:  e.Payload.ToolCallID,
				ToolName:    e.Payload.ToolName,
				Description: e.Payload.Description,
				Action:      e.Payload.Action,
				Path:        e.Payload.Path,
				Params:      e.Payload.Params,
			},
		}
	case pubsub.Event[proto.PermissionNotification]:
		return pubsub.Event[permission.PermissionNotification]{
			Type: e.Type,
			Payload: permission.PermissionNotification{
				ToolCallID: e.Payload.ToolCallID,
				Granted:    e.Payload.Granted,
				Denied:     e.Payload.Denied,
			},
		}
	case pubsub.Event[proto.QuestionRequest]:
		return pubsub.Event[question.Request]{
			Type: e.Type,
			Payload: question.Request{
				ID:                 e.Payload.ID,
				SessionID:          e.Payload.SessionID,
				ToolCallID:         e.Payload.ToolCallID,
				Questions:          protoQuestionsToDomain(e.Payload.Questions),
				ConfirmTitle:       e.Payload.ConfirmTitle,
				ConfirmDescription: e.Payload.ConfirmDescription,
			},
		}
	case pubsub.Event[proto.QuestionNotification]:
		return pubsub.Event[question.Notification]{
			Type: e.Type,
			Payload: question.Notification{
				BatchID: e.Payload.BatchID,
			},
		}
	case pubsub.Event[proto.Message]:
		return pubsub.Event[message.Message]{
			Type:    e.Type,
			Payload: protoToMessage(e.Payload),
		}
	case pubsub.Event[proto.Session]:
		return pubsub.Event[session.Session]{
			Type:    e.Type,
			Payload: protoToSession(e.Payload),
		}
	case pubsub.Event[proto.ThreadEvent]:
		// The TUI's Update() switches on pubsub.Event[proto.Thread] (see
		// root.go/ui.go and threads_dock.go/thread_indicator.go/
		// thread_completion.go/threads.go), not the domain thread.Event —
		// mirror AppWorkspace.translateEvent's local-mode conversion here
		// so both modes feed the same live-update path instead of only
		// the TTL-poll fallback.
		return pubsub.Event[proto.Thread]{
			Type:    threadEventPubsubType(e.Payload.Type),
			Payload: e.Payload.Thread,
		}
	case pubsub.Event[proto.File]:
		return pubsub.Event[history.File]{
			Type:    e.Type,
			Payload: protoToFile(e.Payload),
		}
	case pubsub.Event[proto.AgentEvent]:
		n := notify.Notification{
			SessionID:    e.Payload.SessionID,
			SessionTitle: e.Payload.SessionTitle,
			RunID:        e.Payload.RunID,
			Type:         notify.Type(e.Payload.Type),
			AWSSOCommand: e.Payload.AWSSOCommand,
			AWSSOURL:     e.Payload.AWSSOURL,
		}
		if e.Payload.Error != nil {
			n.Message = e.Payload.Error.Error()
		}
		return pubsub.Event[notify.Notification]{
			Type:    e.Type,
			Payload: n,
		}
	case pubsub.Event[proto.RunComplete]:
		// Translate the wire-level proto.RunComplete back into the
		// agent's domain notify.RunComplete. Without this case the
		// default branch below warns on every run completion in the
		// server-mode TUI, even though the TUI itself doesn't act
		// on RunComplete — converting silently keeps the workspace
		// event bridge symmetric with the server-side wrapEvent.
		return pubsub.Event[notify.RunComplete]{
			Type: e.Type,
			Payload: notify.RunComplete{
				SessionID: e.Payload.SessionID,
				RunID:     e.Payload.RunID,
				MessageID: e.Payload.MessageID,
				Text:      e.Payload.Text,
				Error:     e.Payload.Error,
				Cancelled: e.Payload.Cancelled,
			},
		}
	case pubsub.Event[proto.SkillsEvent]:
		states := protoToSkillStates(e.Payload.States)
		if w.skills != nil {
			w.skills.SetLatestStates(states)
		}
		return pubsub.Event[skills.Event]{
			Type:    e.Type,
			Payload: skills.Event{States: states},
		}
	case pubsub.Event[proto.UpdateAvailable]:
		return app.UpdateAvailableMsg{
			CurrentVersion: e.Payload.CurrentVersion,
			LatestVersion:  e.Payload.LatestVersion,
			IsDevelopment:  e.Payload.IsDevelopment,
		}
	case pubsub.Event[proto.ServerNotice]:
		// Passed straight through: the wire shape already matches what
		// local (in-process) workspaces publish, so the UI's conversion
		// to util.InfoMsg is the same in both modes.
		return e
	default:
		slog.Warn("Unknown event type in translateEvent", "type", fmt.Sprintf("%T", ev))
		return nil
	}
}

func protoToMCPEventType(t proto.MCPEventType) mcp.EventType {
	switch t {
	case proto.MCPEventStateChanged:
		return mcp.EventStateChanged
	case proto.MCPEventToolsListChanged:
		return mcp.EventToolsListChanged
	case proto.MCPEventPromptsListChanged:
		return mcp.EventPromptsListChanged
	case proto.MCPEventResourcesListChanged:
		return mcp.EventResourcesListChanged
	default:
		return mcp.EventStateChanged
	}
}

// protoToSession converts a wire-level proto.Session into the domain
// session.Session. Fields that exist only on the wire (computed-on-read
// signals like IsBusy, and any future presence counters) are
// intentionally dropped here: session.Session models persisted state,
// not transient runtime signals. UI features that need those signals
// should either extend session.Session or read them from the proto
// payload directly before this conversion runs.
func protoToSession(s proto.Session) session.Session {
	return session.Session{
		ID:               s.ID,
		ParentSessionID:  s.ParentSessionID,
		Title:            s.Title,
		SummaryMessageID: s.SummaryMessageID,
		MessageCount:     s.MessageCount,
		PromptTokens:     s.PromptTokens,
		CompletionTokens: s.CompletionTokens,
		Cost:             s.Cost,
		Todos:            protoToTodos(s.Todos),
		CreatedAt:        s.CreatedAt,
		UpdatedAt:        s.UpdatedAt,
	}
}

func protoToTodos(todos []proto.Todo) []session.Todo {
	if len(todos) == 0 {
		return nil
	}
	out := make([]session.Todo, len(todos))
	for i, t := range todos {
		out[i] = session.Todo{
			Content:    t.Content,
			Status:     session.TodoStatus(t.Status),
			ActiveForm: t.ActiveForm,
		}
	}
	return out
}

func protoToFile(f proto.File) history.File {
	return history.File{
		ID:        f.ID,
		SessionID: f.SessionID,
		Path:      f.Path,
		Content:   f.Content,
		Version:   f.Version,
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	}
}

// protoToMessage converts a wire [proto.Message] to a [message.Message].
// This used to be a 9-way type switch rebuilding every content part by
// hand; now that proto's content part types are aliases of message's
// (see proto/message.go), a [proto.Message]'s Parts slice already *is*
// a []message.ContentPart, so there's nothing left to convert.
func protoToMessage(m proto.Message) message.Message {
	return message.Message{
		ID:        m.ID,
		SessionID: m.SessionID,
		Role:      m.Role,
		Parts:     m.Parts,
		Model:     m.Model,
		Provider:  m.Provider,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}

func protoToMessages(msgs []proto.Message) []message.Message {
	out := make([]message.Message, len(msgs))
	for i, m := range msgs {
		out[i] = protoToMessage(m)
	}
	return out
}

func protoToFiles(files []proto.File) []history.File {
	out := make([]history.File, len(files))
	for i, f := range files {
		out[i] = protoToFile(f)
	}
	return out
}

func sessionToProto(s session.Session) proto.Session {
	return proto.Session{
		ID:               s.ID,
		ParentSessionID:  s.ParentSessionID,
		Title:            s.Title,
		SummaryMessageID: s.SummaryMessageID,
		MessageCount:     s.MessageCount,
		PromptTokens:     s.PromptTokens,
		CompletionTokens: s.CompletionTokens,
		Cost:             s.Cost,
		Todos:            todosToProto(s.Todos),
		CreatedAt:        s.CreatedAt,
		UpdatedAt:        s.UpdatedAt,
	}
}

// protoToSkillStates reconstructs internal skill state slices from
// their wire representation. Non-empty Error strings are turned into
// synthetic error values; the TUI never type-asserts on Err.
func protoToSkillStates(in []proto.SkillState) []*skills.SkillState {
	if len(in) == 0 {
		return nil
	}
	out := make([]*skills.SkillState, len(in))
	for i, s := range in {
		state := &skills.SkillState{
			Name:  s.Name,
			Path:  s.Path,
			State: skills.DiscoveryState(s.State),
		}
		if s.Error != "" {
			state.Err = errors.New(s.Error)
		}
		out[i] = state
	}
	return out
}

func todosToProto(todos []session.Todo) []proto.Todo {
	if len(todos) == 0 {
		return nil
	}
	out := make([]proto.Todo, len(todos))
	for i, t := range todos {
		out[i] = proto.Todo{
			Content:    t.Content,
			Status:     string(t.Status),
			ActiveForm: t.ActiveForm,
		}
	}
	return out
}

func protoQuestionsToDomain(qs []proto.QuestionItem) []question.Question {
	if len(qs) == 0 {
		return nil
	}
	out := make([]question.Question, len(qs))
	for i, q := range qs {
		choices := make([]question.Choice, len(q.Choices))
		for j, c := range q.Choices {
			choices[j] = question.Choice{
				ID:          c.ID,
				Label:       c.Label,
				Description: c.Description,
			}
		}
		out[i] = question.Question{
			ID:          q.ID,
			Type:        question.Type(q.Type),
			Label:       q.Label,
			Text:        q.Question,
			Description: q.Description,
			Choices:     choices,
		}
	}
	return out
}
