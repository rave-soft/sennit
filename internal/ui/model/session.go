package model

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// sessionState holds the active session and the bookkeeping around loading,
// continuing, and navigating between sessions.
type SessionFile = workspace.SessionFile

type sessionState struct {
	current *session.Session
	files   []SessionFile
	// filesVersion bumps every time files is replaced. The sidebar cache
	// (sidebar.go) keys off it instead of diffing the slice itself, since
	// files is always assigned wholesale when it changes.
	filesVersion int

	// keeps track of read files while we don't have a session id
	fileReads []string

	// initialSessionID is set when loading a specific session on startup.
	initialSessionID string
	// continueLastSession is set to continue the most recent session on startup.
	continueLastSession bool

	lastUserMessageTime int64

	// modelUsed is the model the loaded session's own assistant messages
	// were produced by — which is not the selected model for a sub-agent's
	// session or one opened from history. See viewedModel.
	modelUsed sessionModelRef

	loadGen        uint64
	loadExpectedID string

	// dialogLoading / dialogGen track the off-thread
	// ListSessions fetch dispatched by openSessionsDialog; see
	// sessionsLoadedMsg.
	dialogLoading bool
	dialogGen     uint64

	// navStack tracks sub-agent session navigation: each frame records
	// where alt+up should return to and the sibling delegations
	// alt+left/alt+right can cycle through, without re-scanning the
	// (possibly no-longer-loaded) parent chat. See enterChildSession.
	navStack []sessionNavFrame
}

// sessionFilesUpdatesMsg is sent when the files for this session have been
// updated. sessionID identifies which session the load was for, so a stale
// reply arriving after the user switched sessions can be dropped instead of
// clobbering m.sess.files with another session's file list.
type sessionFilesUpdatesMsg struct {
	sessionID    string
	sessionFiles []SessionFile
}

// createSessionMsg carries a newly created session and the captured send
// parameters so that Update can apply the session creation and then
// dispatch the AgentRun cmd.
type createSessionMsg struct {
	session     session.Session
	content     string
	attachments []message.Attachment
	generation  uint64
}

func (m *UI) requestSessionLoad(sessionID string) tea.Cmd {
	return m.beginSessionLoad(sessionID)
}

func (m *UI) beginSessionLoad(sessionID string) tea.Cmd {
	m.sess.loadGen++
	if m.sess.loadExpectedID != "" && m.sess.loadExpectedID != sessionID {
		m.editor.pendingSendQueue = nil
		m.editor.pendingSendActive = false
	}
	m.sess.loadExpectedID = sessionID
	generation := m.sess.loadGen
	ctx := m.com.Context()
	workspace := m.com.Workspace
	sessionChanges := m.com.SessionChanges
	styles := m.com.Styles
	// Read here, on the Update goroutine, rather than inside the command:
	// both enterChildSession and exitChildSession adjust the nav stack
	// before asking for the load, so this is already the depth the load is
	// for. See sessionLoadResolver.resumable.
	resumable := !m.viewingChildSession()
	owner := m
	return func() tea.Msg {
		loader := sessionLoadResolver{
			ctx:            ctx,
			workspace:      workspace,
			sessionChanges: sessionChanges,
			styles:         styles,
			config:         workspace.Config(),
			resumable:      resumable,
			owner:          owner,
		}
		return loader.resolve(sessionID, generation)
	}
}

// loadSessionMsg is a message indicating that a session and its files have
// been loaded.
type loadSessionMsg struct {
	uiOwned

	gen                 uint64
	sessionID           string
	session             *session.Session
	files               []SessionFile
	readFiles           []string
	items               []chat.MessageItem
	lastUserMessageTime int64
	modelUsed           sessionModelRef
	// modelSwitched reports that loading this session moved the instance
	// onto the model the session is pinned to, so the memoized model state
	// has to be re-probed. False whenever the session was already on it, or
	// pins none — see workspace.AgentController.ApplySessionModel.
	modelSwitched bool
	err           error
}

type requestSessionLoad struct {
	sessionID string
}

// lspFilePaths returns deduplicated file paths from both modified and read
// files for starting LSP servers.
func (msg loadSessionMsg) lspFilePaths() []string {
	seen := make(map[string]struct{}, len(msg.files)+len(msg.readFiles))
	paths := make([]string, 0, len(msg.files)+len(msg.readFiles))
	for _, f := range msg.files {
		p := f.LatestVersion.Path
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for _, p := range msg.readFiles {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	return paths
}

type sessionLoadResolver struct {
	ctx            context.Context
	workspace      workspace.Workspace
	sessionChanges workspace.SessionChangePreparer
	styles         *styles.Styles
	config         *config.Config
	// resumable marks a load the user can go on to type into: a top-level
	// session, not a sub-agent's transcript they drilled into. Only such a
	// load restores the session's pinned model, because only such a load
	// is followed by a turn that would run on it — see resolve.
	resumable bool
	// owner is the UI this load was started for, carried onto every
	// loadSessionMsg below so Root can deliver the result to it whatever
	// screen is on top by then. Never dereferenced off the Update
	// goroutine — see uiOwnedMsg.
	owner *UI
}

func (r sessionLoadResolver) resolve(sessionID string, gen uint64) tea.Msg {
	s, err := r.workspace.GetSession(r.ctx, sessionID)
	if err != nil {
		return loadSessionMsg{uiOwned: uiOwned{owner: r.owner}, gen: gen, sessionID: sessionID, err: err}
	}
	// Put the session back on the model it was working with before any of
	// it is rendered, so the model shown and the model the next turn runs
	// on are the same one.
	//
	// Only for a session the user can go on to type into. Drilling into a
	// sub-agent loads its transcript through here too, and that transcript
	// is read-only (see enterChildSession): there is no next turn to line
	// the model up with, so switching would only take the user's own model
	// away — a sub-agent commonly runs on a different one — and rebuild the
	// coordinator underneath work that is still going, because they looked
	// at something.
	//
	// A failure here is not a failure to open the session: it opens on the
	// current model, which is what it did before sessions carried a model
	// at all. Refusing to show a session because its model could not be
	// restored would be strictly worse than showing it.
	var modelSwitched bool
	if r.resumable {
		modelSwitched, err = r.workspace.ApplySessionModel(r.ctx, sessionID)
		if err != nil {
			slog.Debug("Failed to restore the session's model", "session_id", sessionID, "error", err)
		}
	}
	sessionFiles, err := loadModifiedFiles(r.ctx, r.sessionChanges, sessionID)
	if err != nil {
		return loadSessionMsg{uiOwned: uiOwned{owner: r.owner}, gen: gen, sessionID: sessionID, err: err}
	}
	readFiles, err := r.workspace.FileTrackerListReadFiles(r.ctx, sessionID)
	if err != nil {
		slog.Error("Failed to load read files for session", "error", err)
	}
	msgs, err := r.workspace.ListMessages(r.ctx, sessionID)
	if err != nil {
		return loadSessionMsg{uiOwned: uiOwned{owner: r.owner}, gen: gen, sessionID: sessionID, err: err}
	}
	items, lastUserMessageTime := sessionMessageItems(r.styles, r.config, msgs)
	if err := loadNestedToolCalls(r.ctx, r.workspace, r.styles, r.config, sessionID, gen, items); err != nil {
		return loadSessionMsg{uiOwned: uiOwned{owner: r.owner}, gen: gen, sessionID: sessionID, err: err}
	}

	return loadSessionMsg{
		uiOwned:             uiOwned{owner: r.owner},
		gen:                 gen,
		sessionID:           sessionID,
		session:             &s,
		files:               sessionFiles,
		readFiles:           readFiles,
		items:               items,
		lastUserMessageTime: lastUserMessageTime,
		modelUsed:           lastAssistantModel(sessionID, msgs),
		modelSwitched:       modelSwitched,
	}
}

// reportCurrentSession returns a fire-and-forget tea.Cmd that
// informs the workspace which session this client is currently
// viewing. Errors are logged at debug only; the call is a hint
// for server-side presence tracking, not correctness-critical
// state.
func (s *sessionState) reportCurrentSession(com *common.Common, sessionID string) tea.Cmd {
	workspace := com.Workspace
	generation := s.loadGen
	ctx := com.Context()
	return func() tea.Msg {
		if err := workspace.SetCurrentSessionGeneration(ctx, sessionID, generation); err != nil {
			slog.Debug("Failed to report current session", "session_id", sessionID, "error", err)
		}
		return nil
	}
}

func loadModifiedFiles(ctx context.Context, preparer workspace.SessionChangePreparer, sessionID string) ([]SessionFile, error) {
	if preparer == nil {
		return nil, fmt.Errorf("session change preparer is unavailable")
	}
	return preparer.PrepareSessionChanges(ctx, sessionID)
}

// handleFileEvent processes file change events and updates the session file
// list with new or updated file information.
func (s *sessionState) handleFileEvent(com *common.Common, file history.File) tea.Cmd {
	if s.current == nil || file.SessionID != s.current.ID {
		return nil
	}

	sessionID := s.current.ID
	ctx, preparer := com.Context(), com.SessionChanges
	return func() tea.Msg {
		sessionFiles, err := loadModifiedFiles(ctx, preparer, sessionID)
		// could not load session files
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return sessionFilesUpdatesMsg{
			sessionID:    sessionID,
			sessionFiles: sessionFiles,
		}
	}
}

func (s *sessionState) refreshModifiedFiles(com *common.Common) tea.Cmd {
	if s.current == nil {
		return nil
	}
	sessionID := s.current.ID
	ctx, preparer := com.Context(), com.SessionChanges
	return func() tea.Msg {
		files, err := loadModifiedFiles(ctx, preparer, sessionID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return sessionFilesUpdatesMsg{sessionID: sessionID, sessionFiles: files}
	}
}

// filesInfo renders the modified files section for the sidebar, showing files
// with their addition/deletion counts.
func (s *sessionState) filesInfo(com *common.Common, cwd string, width, maxItems int, isSection bool) string {
	t := com.Styles

	title := t.Files.SectionTitle.Render("Modified Files")
	if isSection {
		title = common.Section(t, "Modified Files", width)
	}
	list := t.Files.EmptyMessage.Render("None")
	var filesWithChanges []SessionFile
	for _, f := range s.files {
		if !hasFileChanges(f) {
			continue
		}
		filesWithChanges = append(filesWithChanges, f)
	}
	if len(filesWithChanges) > 0 {
		list = fileList(t, cwd, filesWithChanges, width, maxItems)
	}

	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

// fileList renders a list of files with their diff statistics, truncating to
// maxItems and showing a "...and N more" message if needed.
func fileList(t *styles.Styles, cwd string, filesWithChanges []SessionFile, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}
	// The truncation hint is a line of its own, so it has to come out of
	// the budget rather than sit on top of it: rendering maxItems rows
	// *and* the hint made the section one line taller than the caller had
	// allowed for, which pushed the row below it off the panel.
	showLimit := maxItems
	if len(filesWithChanges) > maxItems {
		showLimit = maxItems - 1
	}

	var renderedFiles []string
	filesShown := 0

	for _, f := range filesWithChanges {
		// Skip files with no changes
		if filesShown >= showLimit {
			break
		}

		// Build stats string with colors
		var statusParts []string
		if f.Additions > 0 {
			statusParts = append(statusParts, t.Files.Additions.Render(fmt.Sprintf("+%d", f.Additions)))
		}
		if f.Deletions > 0 {
			statusParts = append(statusParts, t.Files.Deletions.Render(fmt.Sprintf("-%d", f.Deletions)))
		}
		extraContent := strings.Join(statusParts, " ")

		// Format file path
		filePath := f.FirstVersion.Path
		if rel, err := filepath.Rel(cwd, filePath); err == nil {
			filePath = rel
		}
		filePath = fsext.DirTrim(filePath, 2)
		suffix := ""
		if extraContent != "" {
			suffix = " " + extraContent
		}
		maxPathWidth := max(width-lipgloss.Width(suffix), 0)
		// Left truncation: DirTrim above has already shortened the middle
		// directories, so what is left to give up is the head — the file
		// name is the whole point of the row.
		filePath = presentation.TruncatePath(filePath, maxPathWidth)

		line := t.Files.Path.Render(filePath)
		if extraContent != "" {
			line = fmt.Sprintf("%s %s", line, extraContent)
		}

		renderedFiles = append(renderedFiles, line)
		filesShown++
	}

	if remaining := len(filesWithChanges) - filesShown; remaining > 0 {
		renderedFiles = append(renderedFiles, t.Files.TruncationHint.Render(fmt.Sprintf("…and %d more", remaining)))
	}

	return lipgloss.JoinVertical(lipgloss.Left, renderedFiles...)
}

// startLSPs starts LSP servers for the given file paths.
func (m *UI) startLSPs(paths []string) tea.Cmd {
	if len(paths) == 0 {
		return nil
	}

	ctx := m.com.Context()
	ws := m.com.Workspace
	return func() tea.Msg {
		for _, path := range paths {
			ws.LSPStart(ctx, path)
		}
		return nil
	}
}
