package model

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/diff"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

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
	styles := m.com.Styles
	// Read here, on the Update goroutine, rather than inside the command:
	// both enterChildSession and exitChildSession adjust the nav stack
	// before asking for the load, so this is already the depth the load is
	// for. See sessionLoadResolver.resumable.
	resumable := !m.viewingChildSession()
	return func() tea.Msg {
		loader := sessionLoadResolver{
			ctx:       ctx,
			workspace: workspace,
			styles:    styles,
			config:    workspace.Config(),
			resumable: resumable,
		}
		return loader.resolve(sessionID, generation)
	}
}

// loadSessionMsg is a message indicating that a session and its files have
// been loaded.
type loadSessionMsg struct {
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

// SessionFile tracks the first and latest versions of a file in a session,
// along with the total additions and deletions.
type SessionFile struct {
	FirstVersion  history.File
	LatestVersion history.File
	Additions     int
	Deletions     int
	Uncommitted   bool
}

func uncommittedSessionFiles(sessionFiles []SessionFile, files []git.FileChange) []SessionFile {
	uncommitted := make(map[string]struct{}, len(files))
	for _, file := range files {
		uncommitted[filepath.Clean(file.Path)] = struct{}{}
	}

	result := make([]SessionFile, 0, len(sessionFiles))
	for _, file := range sessionFiles {
		if _, ok := uncommitted[filepath.Clean(file.FirstVersion.Path)]; !ok {
			continue
		}
		file.Uncommitted = true
		result = append(result, file)
	}
	return result
}

type sessionLoadResolver struct {
	ctx       context.Context
	workspace workspace.Workspace
	styles    *styles.Styles
	config    *config.Config
	// resumable marks a load the user can go on to type into: a top-level
	// session, not a sub-agent's transcript they drilled into. Only such a
	// load restores the session's pinned model, because only such a load
	// is followed by a turn that would run on it — see resolve.
	resumable bool
}

func (r sessionLoadResolver) resolve(sessionID string, gen uint64) tea.Msg {
	s, err := r.workspace.GetSession(r.ctx, sessionID)
	if err != nil {
		return loadSessionMsg{gen: gen, sessionID: sessionID, err: err}
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
	sessionFiles, err := loadModifiedFiles(r.ctx, r.workspace, sessionID)
	if err != nil {
		return loadSessionMsg{gen: gen, sessionID: sessionID, err: err}
	}
	readFiles, err := r.workspace.FileTrackerListReadFiles(r.ctx, sessionID)
	if err != nil {
		slog.Error("Failed to load read files for session", "error", err)
	}
	msgs, err := r.workspace.ListMessages(r.ctx, sessionID)
	if err != nil {
		return loadSessionMsg{gen: gen, sessionID: sessionID, err: err}
	}
	items, lastUserMessageTime := sessionMessageItems(r.styles, r.config, msgs)
	if err := loadNestedToolCalls(r.ctx, r.workspace, r.styles, r.config, sessionID, gen, items); err != nil {
		return loadSessionMsg{gen: gen, sessionID: sessionID, err: err}
	}

	return loadSessionMsg{
		gen:                 gen,
		sessionID:           sessionID,
		session:             &s,
		files:               sessionFiles,
		readFiles:           readFiles,
		items:               items,
		lastUserMessageTime: lastUserMessageTime,
		modelUsed:           lastAssistantModel(msgs),
		modelSwitched:       modelSwitched,
	}
}

// reportCurrentSession returns a fire-and-forget tea.Cmd that
// informs the workspace which session this client is currently
// viewing. Errors are logged at debug only; the call is a hint
// for server-side presence tracking, not correctness-critical
// state.
func (m *UI) reportCurrentSession(sessionID string) tea.Cmd {
	workspace := m.com.Workspace
	generation := m.sess.loadGen
	return func() tea.Msg {
		if err := workspace.SetCurrentSessionGeneration(context.Background(), sessionID, generation); err != nil {
			slog.Debug("Failed to report current session", "session_id", sessionID, "error", err)
		}
		return nil
	}
}

func sessionFilesFromHistory(files []history.File) []SessionFile {
	filesByPath := make(map[string][]history.File)
	for _, f := range files {
		filesByPath[f.Path] = append(filesByPath[f.Path], f)
	}
	sessionFiles := make([]SessionFile, 0, len(filesByPath))
	for _, versions := range filesByPath {
		if len(versions) == 0 {
			continue
		}

		first := versions[0]
		last := versions[0]
		for _, v := range versions {
			if v.Version < first.Version {
				first = v
			}
			if v.Version > last.Version {
				last = v
			}
		}

		_, additions, deletions := diff.GenerateDiff(first.Content, last.Content, first.Path)

		sessionFiles = append(sessionFiles, SessionFile{
			FirstVersion:  first,
			LatestVersion: last,
			Additions:     additions,
			Deletions:     deletions,
		})
	}

	slices.SortFunc(sessionFiles, func(a, b SessionFile) int {
		if a.LatestVersion.UpdatedAt > b.LatestVersion.UpdatedAt {
			return -1
		}
		if a.LatestVersion.UpdatedAt < b.LatestVersion.UpdatedAt {
			return 1
		}
		return 0
	})
	return sessionFiles
}

func loadModifiedFiles(ctx context.Context, ws workspace.Workspace, sessionID string) ([]SessionFile, error) {
	files, err := ws.ListSessionHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	sessionFiles := sessionFilesFromHistory(files)
	uncommittedFiles, err := ws.UncommittedFiles(ctx)
	if err != nil {
		slog.Error("Failed to load uncommitted files", "error", err)
	}
	if uncommittedFiles != nil {
		return uncommittedSessionFiles(sessionFiles, uncommittedFiles), nil
	}
	return sessionFiles, nil
}

func (m *UI) loadModifiedFiles(sessionID string) ([]SessionFile, error) {
	return loadModifiedFiles(context.Background(), m.com.Workspace, sessionID)
}

// handleFileEvent processes file change events and updates the session file
// list with new or updated file information.
func (m *UI) handleFileEvent(file history.File) tea.Cmd {
	if m.sess.current == nil || file.SessionID != m.sess.current.ID {
		return nil
	}

	return func() tea.Msg {
		sessionFiles, err := m.loadModifiedFiles(m.sess.current.ID)
		// could not load session files
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return sessionFilesUpdatesMsg{
			sessionFiles: sessionFiles,
		}
	}
}

func (m *UI) refreshModifiedFiles() tea.Cmd {
	if m.sess.current == nil {
		return nil
	}
	sessionID := m.sess.current.ID
	return func() tea.Msg {
		files, err := m.loadModifiedFiles(sessionID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return sessionFilesUpdatesMsg{sessionFiles: files}
	}
}

// filesInfo renders the modified files section for the sidebar, showing files
// with their addition/deletion counts.
func (m *UI) filesInfo(cwd string, width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Files.SectionTitle.Render("Modified Files")
	if isSection {
		title = common.Section(t, "Modified Files", width)
	}
	list := t.Files.EmptyMessage.Render("None")
	var filesWithChanges []SessionFile
	for _, f := range m.sess.files {
		if !f.Uncommitted && f.Additions == 0 && f.Deletions == 0 {
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
	var renderedFiles []string
	filesShown := 0

	for _, f := range filesWithChanges {
		// Skip files with no changes
		if filesShown >= maxItems {
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

	if len(filesWithChanges) > maxItems {
		remaining := len(filesWithChanges) - maxItems
		renderedFiles = append(renderedFiles, t.Files.TruncationHint.Render(fmt.Sprintf("…and %d more", remaining)))
	}

	return lipgloss.JoinVertical(lipgloss.Left, renderedFiles...)
}

// startLSPs starts LSP servers for the given file paths.
func (m *UI) startLSPs(paths []string) tea.Cmd {
	if len(paths) == 0 {
		return nil
	}

	return func() tea.Msg {
		ctx := context.Background()
		for _, path := range paths {
			m.com.Workspace.LSPStart(ctx, path)
		}
		return nil
	}
}
