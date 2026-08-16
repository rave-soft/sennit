package model

import (
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"runtime"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/rave-soft/braid/internal/ui/anim"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/completions"
	"github.com/rave-soft/braid/internal/ui/dialog"
	fimage "github.com/rave-soft/braid/internal/ui/image"
	"github.com/rave-soft/braid/internal/ui/notification"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/rave-soft/braid/internal/workspace"
)

// transparentToggledMsg carries the result of a transparency-toggle config mutation.
type transparentToggledMsg struct {
	Err        error
	Enabled    bool
	generation uint64
}

// themeSetMsg carries the result of persisting a theme selection. The
// palette itself is swapped synchronously when the user picks it, so this
// message only reports whether the choice survived to disk; Previous is the
// palette to fall back to if it did not.
type themeSetMsg struct {
	Err        error
	ID         string
	Previous   string
	generation uint64
}

// compactModeToggledMsg carries the result of a compact-mode config mutation.
type compactModeToggledMsg struct {
	Err        error
	Enabled    bool
	generation uint64
}

// providerConfiguredResult carries the outcome of the async provider-config
// flow (UpdatePreferredModel + init) dispatched by ActionProviderConfigured.
type providerConfiguredResult struct {
	Err        error
	Model      config.SelectedModel
	Onboarding bool
	generation uint64
}

// modelSelectResult carries the outcome of the async model-select flow
// dispatched by handleSelectModel.
type modelSelectResult struct {
	Err        error
	Onboarding bool
	Model      config.SelectedModel
	generation uint64
}

type agentModelInitializedMsg struct {
	Err        error
	Onboarding bool
	Model      config.SelectedModel
	generation uint64
}

type modelSettingUpdatedMsg struct {
	Err        error
	Info       string
	generation uint64
}

// notificationStyleSetMsg carries the result of a notification-style config mutation.
type notificationStyleSetMsg struct {
	Err        error
	Style      string
	generation uint64
}

type yoloToggledMsg struct {
	Err        error
	Enabled    bool
	generation uint64
}

type permissionResponseMsg struct {
	Accepted   bool
	Permission string
	generation uint64
}

// sendPendingQueueMsg advances one pending send after a session load completes.
type sendPendingQueueMsg struct{}

type notificationSentMsg struct{}

// sendQueueItem holds one pending-send entry with generation tracking.
type sendQueueItem struct {
	content        string
	attachments    []message.Attachment
	generation     uint64
	sessionID      string
	loadGeneration uint64
	bang           bool
	isFirstMessage bool
}

// TextareaMaxHeight is the maximum height of the prompt textarea before it
// scrolls internally instead of growing further.
const TextareaMaxHeight = 10

// TextareaMinHeight is the minimum height of the prompt textarea: one row,
// so an empty or single-line editor takes exactly one row (plus the prompt
// prefix rendered inline by the textarea itself) — no reserved blank lines.
const TextareaMinHeight = 1

// uiFocusState represents the current focus state of the UI.
//
// The hard invariant (see internal/ui/AGENTS.md) is: input focus is ALWAYS
// on the prompt editor. There are exactly two exceptions, both entered and
// left through dedicated, deliberate transitions rather than mouse clicks
// or incidental keystrokes:
//
//   - A dialog is open: the dialog owns the cursor outright (see Draw);
//     m.focus itself doesn't need to change for this, since dialogs are
//     checked first regardless of focus. Closing the last dialog forces
//     focus back to uiFocusEditor (or uiFocusMain, if a child session is
//     still being viewed underneath) — see handleDialogMsg.
//   - A child session is being viewed (read-only drill-in): uiFocusMain,
//     which hides the terminal cursor and routes keys to chat navigation
//     instead of the textarea. Neither the mouse nor arrow/vim keys can
//     reach this state anymore — only enterChildSession sets it, and
//     exitChildSession restores uiFocusEditor once the nav stack has been
//     unwound back to a top-level session.
type uiFocusState uint8

// Possible uiFocusState values.
const (
	uiFocusNone uiFocusState = iota
	uiFocusEditor
	uiFocusMain
)

type uiState uint8

// Possible uiState values.
const (
	uiOnboarding uiState = iota
	uiInitialize
	uiLanding
	uiChat
)

type openEditorMsg struct {
	Text string
}

type shellResultMsg struct {
	PendingID  string // ID of the pending ShellItem to update.
	Command    string
	Output     string
	ExitCode   int
	Err        error
	Canceled   bool
	sessionID  string
	generation uint64
}

// shellStreamMsg carries incremental output from a streaming shell command.
type shellStreamMsg struct {
	PendingID string
	Chunk     string
	streamCh  <-chan string // unexported; used to continue draining
}

type (
	// cancelTimerExpiredMsg is sent when the cancel timer expires.
	cancelTimerExpiredMsg struct{}
	// userCommandsLoadedMsg is sent when user commands are loaded.
	userCommandsLoadedMsg struct {
		Commands []commands.CustomCommand
	}
	// mcpPromptsLoadedMsg is sent when mcp prompts are loaded.
	mcpPromptsLoadedMsg struct {
		Prompts []commands.MCPPrompt
	}
	// mcpStateChangedMsg is sent when there is a change in MCP client states.
	mcpStateChangedMsg struct {
		states map[string]workspace.MCPClientInfo
	}
	// sendMessageMsg is sent to send a message.
	// currently only used for mcp prompts.
	sendMessageMsg struct {
		Content     string
		Attachments []message.Attachment
	}

	// closeDialogMsg is sent to close the current dialog.
	closeDialogMsg struct{}

	// showThreadsDashboardMsg requests switching to the threads dashboard
	// screen. Handled by the Root router (root.go); a bare *UI has no
	// dashboard screen of its own, so this falls through Update's default
	// case harmlessly when UI is driven directly (e.g. in tests).
	showThreadsDashboardMsg struct{}

	// copyChatHighlightMsg is sent to copy the current chat highlight to clipboard.
	copyChatHighlightMsg struct{}

	// sessionFilesUpdatesMsg is sent when the files for this session have been updated
	sessionFilesUpdatesMsg struct {
		sessionFiles []SessionFile
	}

	// createSessionMsg carries a newly created session and the captured send
	// parameters so that Update can apply the session creation and then
	// dispatch the AgentRun cmd.
	createSessionMsg struct {
		session     session.Session
		content     string
		attachments []message.Attachment
		generation  uint64
	}
)

// settingsOps holds the generation counters and in-flight flags for the
// settings that apply asynchronously (compact mode, notifications, yolo,
// model, transparency, theme, permission responses). A generation is
// bumped each time an operation starts, and the result handler discards
// any reply whose generation doesn't match the latest one, so a stale
// response from a superseded operation can't clobber a newer one.
type settingsOps struct {
	compactModeGeneration    uint64
	notificationGeneration   uint64
	yoloGeneration           uint64
	modelOperationGeneration uint64
	modelOperationLoading    bool
	notificationLoading      bool
	transparentLoading       bool
	transparentGeneration    uint64
	themeGeneration          uint64
	compactModeLoading       bool
	yoloLoading              bool
	permissionLoading        bool
	permissionGeneration     uint64
	permissionID             string
}

// sessionState holds the active session and the bookkeeping around loading,
// continuing, and navigating between sessions.
type sessionState struct {
	session      *session.Session
	sessionFiles []SessionFile

	// keeps track of read files while we don't have a session id
	sessionFileReads []string

	// initialSessionID is set when loading a specific session on startup.
	initialSessionID string
	// continueLastSession is set to continue the most recent session on startup.
	continueLastSession bool

	lastUserMessageTime int64

	sessionLoadGen        uint64
	sessionLoadExpectedID string

	// sessionsDialogLoading / sessionsDialogGen track the off-thread
	// ListSessions fetch dispatched by openSessionsDialog; see
	// sessionsLoadedMsg.
	sessionsDialogLoading bool
	sessionsDialogGen     uint64

	// navStack tracks sub-agent session navigation: each frame records
	// where alt+up should return to and the sibling delegations
	// alt+left/alt+right can cycle through, without re-scanning the
	// (possibly no-longer-loaded) parent chat. See enterChildSession.
	navStack []sessionNavFrame
}

// layoutState holds the terminal dimensions and the derived compact/details
// layout mode.
type layoutState struct {
	// The width and height of the terminal in cells.
	width  int
	height int
	layout uiLayout

	// forceCompactMode tracks whether compact mode is forced by user toggle
	forceCompactMode bool

	// isCompact tracks whether we're currently in compact layout mode (either
	// by user toggle or auto-switch based on window size)
	isCompact bool

	// detailsOpen tracks whether the details panel is open (in compact mode)
	detailsOpen bool

	isTransparent bool
}

// UI represents the main user interface model.
type UI struct {
	com *common.Common

	sess sessionState

	ops settingsOps

	lay layoutState

	// embedded is true for a UI instance attached to a thread's own
	// workspace rather than the top-level session — it skips
	// onboarding/initialize and doesn't drive the terminal progress bar,
	// since only one UI instance may own those.
	embedded bool

	focus uiFocusState
	state uiState

	keyMap KeyMap
	keyenh tea.KeyboardEnhancementsMsg

	dialog *dialog.Overlay
	status *Status

	// isCanceling tracks whether the user has pressed escape once to cancel.
	isCanceling bool

	// editor holds the prompt textarea, attachments, completions popup
	// state, bang (!) shell-mode flags, and prompt history. See editor.go.
	editor editorState

	header *header

	// sendProgressBar instructs the TUI to send progress bar updates to the
	// terminal.
	sendProgressBar    bool
	progressBarEnabled bool

	// caps hold different terminal capabilities that we query for.
	caps common.Capabilities

	// Active inline editor replaces the textarea when non-nil.
	activeInline dialog.InlineEditor
	// inlineCursor stores the cursor from the last inline editor
	// Draw call, used by the cursor positioning logic below.
	inlineCursor *tea.Cursor

	readyPlaceholder   string
	workingPlaceholder string

	// Chat components
	chat *Chat

	// crumbRoot names the thread this UI is embedded in, for the second
	// crumb of the breadcrumb bar ("main › <crumbRoot> › …"). Empty on the
	// top-level UI, which has no thread above it. Set by the router when it
	// attaches (see Root.handleThreadAttached).
	crumbRoot string

	// breadcrumbHover is set while the pointer is over the breadcrumb bar's
	// Back button, for hover feedback.
	breadcrumbHover bool
	// breadcrumbButtonRect is the screen area of that button as last
	// painted. Hit-testing recomputes it instead (see
	// breadcrumbButtonHit) — this is kept only so the bar can be inspected
	// after a draw.
	breadcrumbButtonRect image.Rectangle

	// onboarding state
	onboarding struct {
		yesInitializeSelected bool
	}

	// lsp holds the memoized workspace LSP state and per-server diagnostic
	// counts. See lsp.go.
	lsp lspState

	// mcp
	mcpStates map[string]workspace.MCPClientInfo

	// skills
	skillStates []*skills.SkillState

	// sidebar holds virtual-scroll state and cached rendered content for the
	// chat sidebar. See sidebar.go.
	sidebar sidebarState

	// Notification state
	notifyBackend       notification.Backend
	notifyWindowFocused bool
	// custom commands & mcp commands
	customCommands []commands.CustomCommand
	mcpPrompts     []commands.MCPPrompt

	// panel holds the expand state of the merged session panel (threads +
	// todos + queue, between chat and the editor). See session_panel.go.
	panel sessionPanelState

	// threadLastStatus tracks each thread's last-seen status, so
	// notifyThreadCompletion (thread_completion.go) can detect the exact
	// edge transition into a terminal state and toast it exactly once.
	threadLastStatus map[string]string

	// wsCache holds the memoized workspace busy/yolo/ready/model/queue
	// state and its TTL-cache bookkeeping. See workspace_cache.go.
	wsCache workspaceCacheState

	// threadIndicator holds the memoized active-thread count shown as a
	// header badge. See thread_indicator.go.
	threadIndicator threadIndicatorState

	// threadsDock holds the memoized thread list and per-thread live
	// activity shown by the session panel's threads section. See
	// threads_dock.go / session_panel.go.
	threadsDock threadsDockState

	// mouse highlighting related state
	lastClickTime time.Time
	hoverX        int
	hoverY        int
}

// Option configures a [UI] instance at construction time.
type Option func(*UI)

// WithEmbedded marks the UI as attached to a thread's own workspace rather
// than the top-level session (see the embedded field doc). Used by the Root
// router when attaching to a thread.
func WithEmbedded() Option {
	return func(m *UI) { m.embedded = true }
}

// WithBreadcrumbRoot names the thread this UI is embedded in, so the
// breadcrumb bar can show the path that leads here (see crumbRoot).
func WithBreadcrumbRoot(name string) Option {
	return func(m *UI) { m.crumbRoot = name }
}

// surfacesThreads reports whether this UI shows other threads at all: the
// panel's threads block, the header's active-thread badge, and the
// refreshes that feed them.
//
// A thread's own embedded UI does not. Threads belong to the workspace
// above it — listing its siblings inside one of them says nothing about
// the work you drilled in to look at, and offering to open them from
// there invites a stack of threads within threads. The panel is for what
// this session is doing.
//
// This is also what keeps the messages those refreshes produce
// unambiguous: with only the main UI ever asking, a thread-panel result
// can be routed to its owner (see Root.Update) instead of to whichever
// screen happens to be on top.
func (m *UI) surfacesThreads() bool {
	return !m.embedded
}

// New creates a new instance of the [UI] model.
func New(com *common.Common, initialSessionID string, continueLast bool, opts ...Option) *UI {
	// Editor components
	ta := textarea.New()
	ta.SetStyles(com.Styles.Editor.Textarea)
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	ta.SetVirtualCursor(false)
	ta.DynamicHeight = true
	ta.MinHeight = TextareaMinHeight
	ta.MaxHeight = TextareaMaxHeight
	ta.Focus()

	scrollbarMode := config.ScrollbarDefault
	if cfg := com.Config(); cfg.Options.TUI != nil && cfg.Options.TUI.Scrollbar != "" {
		scrollbarMode = cfg.Options.TUI.Scrollbar
	}
	ch := NewChat(com, scrollbarMode)

	cfg := com.Config()
	var keybindings map[string][]string
	if cfg.Options != nil && cfg.Options.TUI != nil {
		keybindings = cfg.Options.TUI.Keybindings
	}
	keyMap := configuredKeyMap(runtime.GOOS, keybindings)

	// Completions component
	comp := completions.New(completions.PopupStyles{
		Normal:         com.Styles.Completions.Normal,
		Focused:        com.Styles.Completions.Focused,
		Match:          com.Styles.Completions.Match,
		Muted:          com.Styles.Completions.Muted,
		Border:         com.Styles.Completions.Border,
		ScrollbarThumb: com.Styles.Dialog.ScrollbarThumb,
		ScrollbarTrack: com.Styles.Dialog.ScrollbarTrack,
	})

	panelSpinner := spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(com.Styles.Pills.TodoSpinner),
	)

	// Attachments component
	attachments := attachments.New(
		attachments.NewRenderer(
			com.Styles.Attachments.Normal,
			com.Styles.Attachments.Deleting,
			com.Styles.Attachments.Image,
			com.Styles.Attachments.Text,
			com.Styles.Attachments.Skill,
			com.Styles.Attachments.Remove,
			com.Styles.Attachments.RemoveHover,
		),
		attachments.Keymap{
			DeleteMode: keyMap.Editor.AttachmentDeleteMode,
			DeleteAll:  keyMap.Editor.DeleteAllAttachments,
			Escape:     keyMap.Editor.Escape,
		},
	)

	header := newHeader(com)

	ui := &UI{
		com:    com,
		dialog: dialog.NewOverlay(),
		keyMap: keyMap,
		editor: editorState{
			textarea:    ta,
			completions: comp,
			attachments: attachments,
		},
		chat:   ch,
		header: header,
		panel: sessionPanelState{
			panelSpinner:       panelSpinner,
			hoveredPanelThread: -1,
		},
		lsp: lspState{
			lspStates: make(map[string]workspace.LSPClientInfo),
		},
		mcpStates:           make(map[string]workspace.MCPClientInfo),
		notifyBackend:       notification.NoopBackend{},
		notifyWindowFocused: true,
		sess: sessionState{
			initialSessionID:    initialSessionID,
			continueLastSession: continueLast,
		},
		skillStates: skills.GetLatestStates(),
	}
	for _, opt := range opts {
		opt(ui)
	}

	status := NewStatus(com, ui)

	// Seed the yolo cache once at construction; afterwards it is kept
	// fresh by write-through toggles and off-thread refreshes so Update
	// and View never probe the workspace synchronously.
	yolo := com.Workspace.PermissionSkipRequests()
	ui.wsCache.yoloCache.set(yolo)

	// Seed the memoized agent ready/model state the same way so the first
	// frame renders the model info; the busy probe keeps it fresh
	// afterwards.
	if com.Workspace.AgentIsReady() {
		ui.wsCache.agentReady = true
		ui.wsCache.agentModel = com.Workspace.AgentModel()
	}
	ui.setEditorPrompt(yolo)
	ui.randomizePlaceholders()
	ui.editor.textarea.Placeholder = ui.readyPlaceholder
	ui.status = status

	// Initialize compact mode from config
	ui.lay.forceCompactMode = com.Config().Options.TUI.CompactMode

	// set onboarding state defaults
	ui.onboarding.yesInitializeSelected = true

	desiredState := uiLanding
	desiredFocus := uiFocusEditor
	if ui.embedded {
		// A thread's embedded chat always lands directly in uiLanding —
		// onboarding/initialize are one-time, top-level-session concerns.
	} else if !com.Config().IsConfigured() {
		desiredState = uiOnboarding
	} else if n, _ := com.Workspace.ProjectNeedsInitialization(); n {
		desiredState = uiInitialize
	}

	// set initial state
	ui.setState(desiredState, desiredFocus)

	cfgOpts := com.Config().Options

	// disable indeterminate progress bar
	ui.progressBarEnabled = cfgOpts.Progress == nil || *cfgOpts.Progress
	// enable transparent mode
	ui.lay.isTransparent = cfgOpts.TUI.Transparent != nil && *cfgOpts.TUI.Transparent
	if ui.embedded {
		// Only one UI instance may own the terminal's progress bar.
		ui.progressBarEnabled = false
	}

	return ui
}

// KeyMap returns the UI's key bindings. Exposed so the Root router (root.go)
// can recognize app-wide keys (e.g. the threads toggle) without duplicating
// the binding.
func (m *UI) KeyMap() *KeyMap {
	return &m.keyMap
}

// Init initializes the UI model.
func (m *UI) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.state == uiOnboarding {
		if cmd := m.openProvidersDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// load the user commands async
	cmds = append(cmds, m.loadCustomCommands())
	// load prompt history async
	cmds = append(cmds, m.loadPromptHistory())
	// Prime the memoized LSP state off-thread.
	if cmd := m.requestLSPRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	// load initial session if specified
	if cmd := m.loadInitialSession(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	// Prime the memoized busy/permission state off-thread.
	if cmd := m.dispatchBusyRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	cmds = append(cmds, m.checkPendingMCPAuth())
	if cmd := m.checkConfigProblems(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// checkConfigProblems surfaces config.Doctor's findings as a single
// startup toast (e.g. "3 config problems — /doctor for details") so a
// misconfiguration like a sub-agent pinned to a nonexistent model is
// visible instead of only a log line the user never reads. It only counts
// static problems available immediately after config load; MCP server
// health (which needs a live connection attempt) is picked up by the
// /doctor dialog itself once servers have connected.
func (m *UI) checkConfigProblems() tea.Cmd {
	if m.state == uiOnboarding {
		return nil
	}
	n := len(config.Doctor(m.com.Config()))
	if n == 0 {
		return nil
	}
	noun := "problem"
	if n != 1 {
		noun = "problems"
	}
	return util.ReportWarn(fmt.Sprintf("%d config %s — /doctor for details", n, noun))
}

// loadInitialSession loads the initial session if one was specified on startup.
func (m *UI) loadInitialSession() tea.Cmd {
	switch {
	case m.state != uiLanding:
		// Only load if we're in landing state (i.e., fully configured)
		return nil
	case m.sess.initialSessionID != "":
		return m.requestSessionLoad(m.sess.initialSessionID)
	case m.sess.continueLastSession:
		ws := m.com.Workspace
		return func() tea.Msg {
			sessions, err := ws.ListSessions(context.Background())
			if err != nil || len(sessions) == 0 {
				return nil
			}
			return requestSessionLoad{sessionID: sessions[0].ID}
		}
	default:
		return nil
	}
}

// setState changes the UI state and focus.
func (m *UI) setState(state uiState, focus uiFocusState) {
	if state == uiLanding {
		// Always turn off compact mode when going to landing
		m.lay.isCompact = false
	}
	m.state = state
	m.focus = focus
	// Changing the state may change layout, so update it.
	m.updateLayoutAndSize()
}

// loadCustomCommands loads the custom commands asynchronously.
func (m *UI) loadCustomCommands() tea.Cmd {
	return func() tea.Msg {
		customCommands, err := commands.LoadCustomCommands(m.com.Config())
		if err != nil {
			slog.Error("Failed to load custom commands", "error", err)
		}
		// Append user-invocable skills as commands.
		skillEntries, err := m.com.Workspace.ListSkills(context.Background())
		if err != nil {
			slog.Error("Failed to load skill commands", "error", err)
		}
		customCommands = append(customCommands, commands.FromSkillCatalog(skillEntries)...)
		return userCommandsLoadedMsg{Commands: customCommands}
	}
}

// Update handles updates to the UI model.
func (m *UI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	// Update terminal capabilities
	m.caps.Update(msg)
	if m.dialog.ContainsDialog(dialog.FilePickerID) {
		switch msg := msg.(type) {
		case tea.WindowSizeMsg:
			if cmd := m.applyDialogAction(m.dialog.UpdateDialog(dialog.FilePickerID, dialog.FilePickerUpdateMsg{Capabilities: m.caps, WindowSize: &msg})); cmd != nil {
				cmds = append(cmds, cmd)
			}
		case tea.EnvMsg, uv.PixelSizeEvent, uv.KittyGraphicsEvent:
			if cmd := m.applyDialogAction(m.dialog.UpdateDialog(dialog.FilePickerID, dialog.FilePickerUpdateMsg{Capabilities: m.caps})); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	switch msg := msg.(type) {
	case tea.EnvMsg, tea.ModeReportMsg, uv.UnknownOscEvent, tea.FocusMsg, tea.BlurMsg, tea.WindowSizeMsg, tea.KeyboardEnhancementsMsg, anim.StepMsg, scrollbarHideMsg, chatWarmMsg, sidebarScrollbarHideMsg, spinner.TickMsg, uv.KittyGraphicsEvent:
		var done bool
		if cmds, done = m.updateSystem(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case sessionsLoadedMsg, busyStateMsg, promptQueueMsg, agentRunSubmittedMsg, loadSessionMsg, requestSessionLoad, sessionFilesUpdatesMsg, sendMessageMsg, pubsub.Event[session.Session], pubsub.Event[message.Message], pubsub.Event[history.File], sendMessageErrorMsg, sendPendingQueueMsg, bangSessionCreatedMsg, createSessionMsg:
		var done bool
		if cmds, done = m.updateSession(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case lspStatesMsg, userCommandsLoadedMsg, mcpStateChangedMsg, mcpPromptsLoadedMsg, promptHistoryLoadedMsg, pubsub.Event[workspace.LSPEvent], pubsub.Event[skills.Event], dialog.ActionMCPAuthStarted, dialog.ActionMCPAuthComplete, dialog.ActionMCPAuthErrored, pubsub.Event[workspace.MCPEvent]:
		var done bool
		if cmds, done = m.updateIntegrations(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case agentModelChangedMsg:
		// The coordinator model changed (selection, thinking, reasoning):
		// re-fetch the memoized ready/model state off-thread.
		m.invalidateBusyCaches()
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case closeDialogMsg, pubsub.Event[permission.PermissionRequest], pubsub.Event[permission.PermissionNotification], pubsub.Event[question.Request], pubsub.Event[question.Notification]:
		var done bool
		if cmds, done = m.updatePrompts(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case providerConfiguredResult, modelSelectResult, agentModelInitializedMsg, modelSettingUpdatedMsg, transparentToggledMsg, themeSetMsg, compactModeToggledMsg, notificationStyleSetMsg, permissionResponseMsg, yoloToggledMsg, notificationSentMsg, importCopilotResult:
		var done bool
		if cmds, done = m.updateSettings(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case tea.TerminalVersionMsg:
		return m.updateTerminalVersion(msg)
	case copyChatHighlightMsg:
		cmds = append(cmds, m.copyChatHighlight())
	case DelayedClickMsg, tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, common.CoalescedWheelMsg:
		var done bool
		if cmds, done = m.updateMouse(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case tea.KeyPressMsg:
		if cmd := m.handleKeyPressMsg(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case tea.PasteMsg:
		if m.activeInline != nil && m.focus == uiFocusEditor {
			if p, ok := m.activeInline.(dialog.PasteableEditor); ok {
				if cmd := p.HandlePaste(msg); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return m, tea.Batch(cmds...)
			}
		}
		if cmd := m.handlePasteMsg(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case openEditorMsg, shellStreamMsg, shellResultMsg:
		var done bool
		if cmds, done = m.updateShell(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case util.InfoMsg, util.ClearStatusMsg, pubsub.Event[proto.ServerNotice], workspace.UpdateAvailableMsg, pubsub.Event[workspace.AgentNotification], cancelTimerExpiredMsg:
		var done bool
		if cmds, done = m.updateStatus(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case pubsub.Event[proto.Thread], threadIndicatorLoadedMsg, threadsDockLoadedMsg, threadDockActivityLoadedMsg:
		var done bool
		if cmds, done = m.updateThreads(msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	case completions.CompletionItemsLoadedMsg:
		if m.editor.completionsOpen {
			m.editor.completions.SetItems(msg.Files, msg.Resources)
		}
	case fimage.PreviewPreparedMsg:
		if action := m.dialog.UpdateDialog(dialog.FilePickerID, msg); action != nil {
			if cmd := m.applyDialogAction(action); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	default:
		if m.dialog.HasDialogs() {
			if cmd := m.handleDialogMsg(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	// This logic gets triggered on any message type, but should it?
	switch m.focus {
	case uiFocusMain:
	case uiFocusEditor:
		// Textarea placeholder logic
		if m.viewingChildSession() {
			m.editor.textarea.Placeholder = "viewing subagent session · " + m.exitChildSessionShortcut() + " to return"
		} else if m.editor.bangMode {
			m.editor.textarea.Placeholder = "Run a shell command"
		} else if m.isAgentBusy() {
			m.editor.textarea.Placeholder = m.workingPlaceholder
		} else {
			m.editor.textarea.Placeholder = m.readyPlaceholder
		}
		if !m.editor.bangMode && m.yoloModeCached() {
			m.editor.textarea.Placeholder = "Yolo mode!"
		}
	}

	// TTL backstop: schedule an off-thread re-probe for any memoized
	// workspace state that has gone stale. Never does IO on this
	// goroutine.
	cmds = append(cmds, m.staleWorkspaceRefreshCmds()...)

	// at this point this can only handle [message.Attachment] message, and we
	// should return all cmds anyway.
	_ = m.editor.attachments.Update(msg)
	return m, tea.Batch(cmds...)
}

// childSessionRef identifies a sub-agent delegation (agent / agentic_fetch
// tool call) that can be entered as its own child session, via
// workspace.Workspace.CreateAgentToolSessionID(messageID, toolCallID).
type childSessionRef struct {
	messageID  string
	toolCallID string
}

// sessionNavFrame is one level of the sub-agent session-navigation stack
// (m.sess.navStack): where alt+up should return to, and the sibling
// delegations in that parent chat that alt+left/alt+right cycle through.
type sessionNavFrame struct {
	parentSessionID string
	// parentTitle is the parent session's title, captured at
	// enterChildSession time. m.sess.session is repointed to the child as soon
	// as navigation starts, so this is the only cheap way to recover the
	// parent's title later (e.g. for the breadcrumb) without extra IO.
	parentTitle string
	// label is the short name of the child session this frame descends
	// into (see childSessionLabel), captured whenever the frame is pushed
	// or its sibling index changes. Used to render the full breadcrumb
	// chain in drawChildSessionPanel without needing the parent's chat
	// items, which usually aren't loaded once navigation has moved on.
	label        string
	siblings     []childSessionRef
	siblingIndex int

	// agentName, model, effort, delegationStart, and delegationDuration
	// mirror the delegation tool item's resolved chat.DelegationInfoProvider
	// data (the same values shown in the collapsed delegation block's
	// subtitle and status line), captured alongside label for the same
	// reason: the parent chat holding that item usually isn't loaded once
	// navigation has moved on. Used by the child-session panel's name/
	// model/effort/elapsed line. agentName/model/effort are "" and
	// delegationStart is the zero time when the item couldn't be resolved
	// (e.g. a cycled-to sibling never rendered in this chat); model/effort
	// are also "" when the delegation has no override (agentic_fetch, or
	// an agent tool using the app's default model). delegationDuration is
	// zero while the delegation is still running (see drawChildSessionPanel,
	// which then computes a live elapsed time from delegationStart instead).
	agentName, model, effort string
	delegationStart          time.Time
	delegationDuration       time.Duration
}

// handleSelectModel performs the model selection after any provider
// pre-checks have completed.  The ImportCopilot, UpdatePreferredModel, and
// initAgentAndReportModel steps run sequentially via typed result messages;
// errors stop the chain without a false success.
func (m *UI) handleSelectModel(msg dialog.ActionSelectModel) tea.Cmd {
	var cmds []tea.Cmd

	if m.ops.modelOperationLoading {
		return util.ReportWarn("Model settings are already being updated")
	}

	// we ignore dialogs with the oauth id as they need to be able to be dismissed
	if m.isAgentBusy() && !m.dialog.ContainsDialog(dialog.OAuthID) {
		return util.ReportWarn("Agent is busy, please wait...")
	}

	cfg := m.com.Config()
	if cfg == nil {
		return util.ReportError(errors.New("configuration not found"))
	}

	var (
		providerID   = msg.Model.Provider
		isCopilot    = providerID == string(catwalk.InferenceProviderCopilot)
		isConfigured = func() bool { _, ok := cfg.Providers.Get(providerID); return ok }
		isOnboarding = m.state == uiOnboarding
	)

	// Attempt to import GitHub Copilot tokens from VSCode if available.
	// ImportCopilot runs first via a typed result; when it lands we
	// re-check whether the provider is configured and decide auth vs model
	// flow — never batch the import with the subsequent steps.
	if isCopilot && !msg.ReAuthenticate {
		m.ops.modelOperationLoading = true
		m.ops.modelOperationGeneration++
		generation := m.ops.modelOperationGeneration
		ws := m.com.Workspace
		cmds = append(cmds, func() tea.Msg {
			ws.ImportCopilot()
			return importCopilotResult{
				providerID:   providerID,
				model:        msg.Model,
				isOnboarding: isOnboarding,
				generation:   generation,
			}
		})
		return tea.Batch(cmds...)
	}

	if !isConfigured() || msg.ReAuthenticate {
		m.dialog.CloseDialog(dialog.ModelsID)
		if cmd := m.openAuthenticationDialog(msg.Provider, msg.Model); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...)
	}

	// Move UpdatePreferredModel into the cmd; the result is handled by a
	// modelSelectResult case that only calls initAgentAndReportModel on success.
	m.ops.modelOperationLoading = true
	m.ops.modelOperationGeneration++
	generation := m.ops.modelOperationGeneration
	capturedModel := msg.Model
	ws := m.com.Workspace
	cmds = append(cmds, func() tea.Msg {
		if err := ws.UpdatePreferredModel(config.ScopeGlobal, capturedModel); err != nil {
			return modelSelectResult{Err: err, generation: generation}
		}
		return modelSelectResult{Onboarding: isOnboarding, Model: capturedModel, generation: generation}
	})

	m.dialog.CloseDialog(dialog.APIKeyInputID)
	m.dialog.CloseDialog(dialog.OAuthID)
	m.dialog.CloseDialog(dialog.ModelsID)

	return tea.Batch(cmds...)
}

// initAgentAndReportModel wires the coder agent up to model (InitCoderAgent
// first when onboarding, since the agent doesn't exist yet) and reports
// which model is now active. Shared by handleSelectModel and
// ActionProviderConfigured's onboarding branch, which both land here right
// after a model has been chosen.
func (m *UI) initAgentAndReportModel(isOnboarding bool, model config.SelectedModel, generation uint64) tea.Cmd {
	ws := m.com.Workspace
	ctx := m.com.Context()
	return m.updateAgentModelCmd(func() tea.Msg {
		// InitCoderAgent brings the coder agent up for the first time
		// (onboarding); it must complete before UpdateAgentModel touches
		// it, so both run in this single off-thread step rather than as
		// separate commands racing each other.
		if isOnboarding {
			if err := ws.InitCoderAgent(ctx); err != nil {
				return agentModelInitializedMsg{Err: err, Onboarding: isOnboarding, Model: model, generation: generation}
			}
		}
		if err := ws.UpdateAgentModel(ctx); err != nil {
			return agentModelInitializedMsg{Err: err, Onboarding: isOnboarding, Model: model, generation: generation}
		}
		return agentModelInitializedMsg{Onboarding: isOnboarding, Model: model, generation: generation}
	})
}

// activeThreadBadgeCount is the header badge's active-thread count, or 0
// where threads are not this UI's business (see surfacesThreads).
func (m *UI) activeThreadBadgeCount() int {
	if !m.surfacesThreads() {
		return 0
	}
	return m.threadIndicator.cache.value
}

func (m *UI) currentModelSupportsImages() bool {
	cfg := m.com.Config()
	if cfg == nil {
		return false
	}
	if _, ok := cfg.Agents[config.AgentCoder]; !ok {
		return false
	}
	// The coder agent leaves Model unset (it inherits the app's configured
	// model), so the model it actually runs on is always cfg.Model.
	model := cfg.SelectedCatalogModel()
	return model != nil && model.SupportsImages
}

// toggleCompactMode toggles compact mode between uiChat and uiChatCompact states.
// The actual SetCompactMode I/O runs inside the returned cmd; the UI state
// is updated only when the result lands via compactModeToggledMsg.
func (m *UI) toggleCompactMode() tea.Cmd {
	if m.ops.compactModeLoading {
		return util.ReportWarn("Compact mode is already being updated")
	}
	desired := !m.lay.forceCompactMode
	m.ops.compactModeLoading = true
	m.ops.compactModeGeneration++
	generation := m.ops.compactModeGeneration
	workspace := m.com.Workspace
	return func() tea.Msg {
		return compactModeToggledMsg{Err: workspace.SetCompactMode(config.ScopeGlobal, desired), Enabled: desired, generation: generation}
	}
}

// uiLayout defines the positioning of UI elements.
type uiLayout struct {
	// area is the overall available area.
	area uv.Rectangle

	// header is the header shown in special cases
	// e.x when the sidebar is collapsed
	// or when in the landing page
	// or in init/config
	header uv.Rectangle

	// main is the area for the main pane. (e.x chat, configure, landing)
	main uv.Rectangle

	// panel is the area for the merged session panel (active threads +
	// todos + queued prompts) between chat and the editor.
	panel uv.Rectangle

	// editor is the area for the editor pane.
	editor uv.Rectangle

	// sidebar is the area for the sidebar.
	sidebar uv.Rectangle

	// status is the area for the status view.
	status uv.Rectangle

	// session details is the area for the session details overlay in compact mode.
	sessionDetails uv.Rectangle
}

// isAgentBusy returns true if the agent coordinator exists and is currently
// busy processing a request. It only reads the memoized state (it runs in
// per-message paths like the textarea placeholder, where a workspace probe
// is treated as IO); the value is refreshed off-thread, see
// workspace_cache.go.
func (m *UI) isAgentBusy() bool {
	if m.editor.bangCancel != nil {
		return true
	}
	return m.wsCache.agentBusyCache.value
}

// hasSession returns true if there is an active session with a valid ID.
func (m *UI) hasSession() bool {
	return m.sess.session != nil && m.sess.session.ID != ""
}

// applyTheme switches the live palette and persists the choice. The swap is
// synchronous so the next frame is already in the new colors; the write to
// disk happens off the Update goroutine and reverts the swap if it fails,
// rather than leaving the UI in a palette the config does not agree with.
func (m *UI) applyTheme(id string) tea.Cmd {
	if !styles.IsKnownPaletteID(id) {
		return util.ReportError(fmt.Errorf("unknown theme %q", id))
	}
	previous := styles.PaletteByID(m.com.Config().ThemeID()).ID
	if id == previous {
		return nil
	}

	m.setTheme(id)
	m.ops.themeGeneration++
	generation := m.ops.themeGeneration
	ws := m.com.Workspace
	return func() tea.Msg {
		return themeSetMsg{
			Err:        ws.SetConfigField(config.ScopeGlobal, "options.tui.theme", id),
			ID:         id,
			Previous:   previous,
			generation: generation,
		}
	}
}

// setTheme replaces the styles every component shares and drops the render
// caches that hold strings colored by the old palette. Most of the UI reads
// com.Styles at draw time and needs nothing; only the caches and the few
// widgets that copied a style at construction do.
func (m *UI) setTheme(id string) {
	*m.com.Styles = styles.Theme(id)

	if m.status != nil {
		m.status.Restyle()
	}
	if m.chat != nil {
		m.chat.InvalidateRenderCaches()
	}
	m.cacheSidebarLogo(m.lay.layout.sidebar.Dx())
	m.updateLayoutAndSize()
}

// attachSkill reads a skill's content by ID and returns it as a markdown
// attachment to be added to the attachment toolbar. The user can then
// compose a message and send it with the skill attached.
// The name parameter is used as a fallback when the server does not
// return one.
func (m *UI) attachSkill(skillID, name string) tea.Cmd {
	return func() tea.Msg {
		content, result, err := m.com.Workspace.ReadSkill(context.Background(), skillID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		fileName := result.Name
		if fileName == "" {
			fileName = name
		}
		return message.Attachment{
			FilePath: fileName,
			FileName: fileName,
			MimeType: "text/markdown",
			Content:  content,
		}
	}
}

// sendAfterSessionLoaded first loads the session and then sends the captured
// message.  It works by chaining through messages so that the command-driving
// harness sees each intermediate step.

// sendMessageErrorMsg carries an error from a sendMessage cmd. The Update
// handler converts it into a util.InfoMsg and clears the optimistic busy
// state (already done inside the cmd).
type importCopilotResult struct {
	providerID   string
	model        config.SelectedModel
	isOnboarding bool
	generation   uint64
}

type sendMessageErrorMsg struct {
	Err            error
	generation     uint64
	sessionID      string
	loadGeneration uint64
	creating       bool
}

// bangSessionCreatedMsg is returned by runShellCommandInternal when a bang
// command triggered a session creation; the Update handler uses it to load
// the session and then starts the shell command.
type bangSessionCreatedMsg struct {
	session        session.Session
	command        string
	isFirstMessage bool
	generation     uint64
}

// sessionsLoadedMsg delivers the result of the off-thread ListSessions
// fetch dispatched by openSessionsDialog. gen guards against a stale fetch
// (superseded by a later open request) opening the dialog after the fact;
// see applySessionsLoaded.
type sessionsLoadedMsg struct {
	gen               uint64
	sessions          []session.Session
	selectedSessionID string
	err               error
}

// newSession clears the current session state and prepares for a new session.
// The actual session creation happens when the user sends their first message.
// Returns a command to reload prompt history.
func (m *UI) newSession() tea.Cmd {
	if !m.hasSession() {
		return nil
	}

	m.sess.sessionLoadGen++
	m.sess.sessionLoadExpectedID = ""
	m.sess.session = nil
	m.sidebar.offset = 0
	m.sess.sessionFiles = nil
	m.sess.sessionFileReads = nil
	m.editor.pendingSendQueue = nil
	m.editor.pendingSendGen = 0
	m.editor.pendingSendLoading = false
	m.setState(uiLanding, uiFocusEditor)
	m.editor.textarea.Focus()
	m.chat.Blur()
	m.chat.ClearMessages()
	m.panel.expanded = false
	m.panel.autoExpanded = false
	m.panel.panelTodosScrollOffset = 0
	m.wsCache.promptQueue = 0
	m.wsCache.promptQueueItems = nil
	m.wsCache.promptQueueCheckedAt = time.Now()
	m.invalidateBusyCaches()
	m.invalidatePromptQueue()
	m.editor.historyReset()
	workspace.ResetAgentToolCache()
	return tea.Batch(
		func() tea.Msg {
			m.com.Workspace.LSPStopAll(context.Background())
			return nil
		},
		m.loadPromptHistory(),
		m.reportCurrentSession(""),
	)
}

func (m *UI) copyChatHighlight() tea.Cmd {
	text := m.chat.HighlightContent()
	return common.CopyToClipboardWithCallback(
		text,
		"Selected text copied to clipboard",
		func() tea.Msg {
			m.chat.ClearMouse()
			return nil
		},
	)
}
