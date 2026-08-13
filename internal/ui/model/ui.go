package model

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/charmbracelet/ultraviolet/screen"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/editor"
	xstrings "github.com/charmbracelet/x/exp/strings"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/notify"
	agenttools "github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/clipboard"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/event"
	"github.com/rave-soft/braid/internal/fsext"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/home"
	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/rave-soft/braid/internal/ui/anim"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/completions"
	"github.com/rave-soft/braid/internal/ui/dialog"
	fimage "github.com/rave-soft/braid/internal/ui/image"
	"github.com/rave-soft/braid/internal/ui/logo"
	"github.com/rave-soft/braid/internal/ui/notification"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/rave-soft/braid/internal/version"
	"github.com/rave-soft/braid/internal/workspace"
)

// transparentToggledMsg carries the result of a transparency-toggle config mutation.
type transparentToggledMsg struct {
	Err        error
	Enabled    bool
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

// configMutationMsg carries the result of a config-mutation cmd.
type configMutationMsg struct {
	Err error
	Msg tea.Msg
}

// Compact mode breakpoints.
const (
	compactModeWidthBreakpoint  = 120
	compactModeHeightBreakpoint = 30
)

// If pasted text has more than 10 newlines, treat it as a file attachment.
const pasteLinesThreshold = 10

// If pasted text has more than 1000 columns, treat it as a file attachment.
const pasteColsThreshold = 1000

// Session details panel max height.
const sessionDetailsMaxHeight = 20

// TextareaMaxHeight is the maximum height of the prompt textarea before it
// scrolls internally instead of growing further.
const TextareaMaxHeight = 10

// TextareaMinHeight is the minimum height of the prompt textarea: one row,
// so an empty or single-line editor takes exactly one row (plus the prompt
// prefix rendered inline by the textarea itself) — no reserved blank lines.
const TextareaMinHeight = 1

// editorAttachmentsRowHeight is the extra row the editor reserves above the
// textarea for the attachments strip. It only counts when there's something
// to show there — see (*UI).editorAttachmentsRowOffset.
const editorAttachmentsRowHeight = 1

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
		states map[string]mcp.ClientInfo
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

// UI represents the main user interface model.
type UI struct {
	com          *common.Common
	session      *session.Session
	sessionFiles []SessionFile

	// keeps track of read files while we don't have a session id
	sessionFileReads []string

	compactModeGeneration    uint64
	notificationGeneration   uint64
	yoloGeneration           uint64
	modelOperationGeneration uint64
	modelOperationLoading    bool
	notificationLoading      bool
	transparentLoading       bool
	transparentGeneration    uint64
	compactModeLoading       bool
	yoloLoading              bool
	permissionLoading        bool
	permissionGeneration     uint64
	permissionID             string

	// initialSessionID is set when loading a specific session on startup.
	initialSessionID string
	// continueLastSession is set to continue the most recent session on startup.
	continueLastSession bool

	lastUserMessageTime int64

	// The width and height of the terminal in cells.
	width  int
	height int
	layout uiLayout

	isTransparent bool

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

	// navStack tracks sub-agent session navigation: each frame records
	// where alt+up should return to and the sibling delegations
	// alt+left/alt+right can cycle through, without re-scanning the
	// (possibly no-longer-loaded) parent chat. See enterChildSession.
	navStack []sessionNavFrame

	// childPanelHover is set while the pointer is over the "back" button in
	// the child-session panel (see drawChildSessionPanel), for hover
	// feedback matching the status bar's back-button pattern.
	childPanelHover bool
	// childPanelButtonRect is the screen area of the panel's "back" button,
	// recomputed on every drawChildSessionPanel call. Used to scope hover
	// feedback to the button itself rather than the whole panel.
	childPanelButtonRect image.Rectangle

	// onboarding state
	onboarding struct {
		yesInitializeSelected bool
	}

	// lspStates / lspDiagnostics memoize the workspace LSP state and
	// per-server severity counts (each probe behind them is a synchronous
	// HTTP round-trip in client/server mode, and the sidebar, landing view,
	// and compact header render them every frame). LSP events refresh them
	// off-thread with a TTL backstop; see lsp.go.
	lspStates        map[string]workspace.LSPClientInfo
	lspDiagnostics   map[string]lsp.DiagnosticCounts
	lspFetchInFlight bool
	// lspRefreshQueued records that an LSP event arrived while a fetch was
	// already in flight; applyLSPStates re-dispatches so the freshest state
	// still lands.
	lspRefreshQueued bool
	lspCheckedAt     time.Time

	// mcp
	mcpStates map[string]mcp.ClientInfo

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

	// forceCompactMode tracks whether compact mode is forced by user toggle
	forceCompactMode bool

	// isCompact tracks whether we're currently in compact layout mode (either
	// by user toggle or auto-switch based on window size)
	isCompact bool

	// detailsOpen tracks whether the details panel is open (in compact mode)
	detailsOpen bool

	// panel holds the expand state of the merged session panel (threads +
	// todos + queue, between chat and the editor). See session_panel.go.
	panel sessionPanelState

	// hoveredPanelThread is the index (into panelThreads/panelThreadRects)
	// of the thread block currently under the pointer, -1 for none. Set
	// from tea.MouseMotionMsg, read by drawSessionPanel for hover styling.
	hoveredPanelThread int
	// panelThreadRects/panelThreads are parallel slices — the on-screen
	// rect and source thread for each currently visible thread block —
	// rebuilt on every drawSessionPanel call. Used by the MouseClickMsg
	// hit-test (drill into the clicked thread) and MouseMotionMsg hover
	// tracking.
	panelThreadRects []uv.Rectangle
	panelThreads     []proto.Thread
	// hoveredPanelDelegation/panelDelegationRects/panelDelegations mirror
	// hoveredPanelThread/panelThreadRects/panelThreads for the delegations
	// section — see runningDelegationBlocks/drawDelegationBlocks in
	// session_panel.go.
	hoveredPanelDelegation int
	panelDelegationRects   []uv.Rectangle
	panelDelegations       []panelDelegation
	// panelTodosHover / panelTodosHeaderRect mirror childPanelHover /
	// childPanelButtonRect for the todos header row's click-to-toggle
	// affordance.
	panelTodosHover      bool
	panelTodosHeaderRect uv.Rectangle
	// panelTodosListRect is the on-screen area of the (possibly scrollable)
	// todo rows below the header, rebuilt on every drawSessionPanel call —
	// the mouse-wheel handler in Update hit-tests against this to decide
	// whether a wheel event scrolls the todos section instead of the chat.
	panelTodosListRect uv.Rectangle
	// panelTodosScrollOffset is the expanded todos section's own scroll
	// position — an index into the concatenated in-progress+pending+done
	// row list (see sessionPanelTodosContent), independent of chat/sidebar
	// scrolling. sessionPanelPlan never drops todosDone/todosPending to fit
	// the panel's row budget anymore (see session_panel.go); when the
	// section's natural size exceeds what's granted, this offset is what
	// reveals the rest instead. drawSessionPanel clamps it to
	// [0, max(0, contentRows-viewportRows)] every frame, so a stale offset
	// left over from a shorter list (todos completed/removed) never
	// dangles out of range.
	panelTodosScrollOffset int

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

	// sessionsDialogLoading / sessionsDialogGen track the off-thread
	// ListSessions fetch dispatched by openSessionsDialog; see
	// sessionsLoadedMsg.
	sessionsDialogLoading bool
	sessionsDialogGen     uint64

	// Todo spinner
	panelSpinner    spinner.Model
	panelIsSpinning bool

	sessionLoadGen        uint64
	sessionLoadExpectedID string

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
		chat:                   ch,
		header:                 header,
		panelSpinner:           panelSpinner,
		lspStates:              make(map[string]workspace.LSPClientInfo),
		mcpStates:              make(map[string]mcp.ClientInfo),
		notifyBackend:          notification.NoopBackend{},
		notifyWindowFocused:    true,
		initialSessionID:       initialSessionID,
		continueLastSession:    continueLast,
		skillStates:            skills.GetLatestStates(),
		hoveredPanelThread:     -1,
		hoveredPanelDelegation: -1,
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
	ui.forceCompactMode = com.Config().Options.TUI.CompactMode

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
	ui.isTransparent = cfgOpts.TUI.Transparent != nil && *cfgOpts.TUI.Transparent
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
	case m.initialSessionID != "":
		return m.requestSessionLoad(m.initialSessionID)
	case m.continueLastSession:
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

// sendNotification returns a command that sends a notification if allowed by policy.
func (m *UI) sendNotification(n notification.Notification) tea.Cmd {
	if !m.shouldSendNotification() {
		return nil
	}
	backend := m.notifyBackend
	return tea.Sequence(backend.Send(n), func() tea.Msg { return notificationSentMsg{} })
}

// maxNotificationBodyLen caps notification body text so OS notification
// centers don't clip or wrap it awkwardly. Long session titles and error
// messages get truncated with an ellipsis.
const maxNotificationBodyLen = 120

// notificationTitle returns the desktop notification title. Appending the
// project directory name lets a user running Braid in several workspaces
// at once tell which one a notification came from; falls back to plain
// "Braid" when the working directory is unknown or root.
func notificationTitle(workingDir string) string {
	name := filepath.Base(filepath.Clean(workingDir))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "Braid"
	}
	return "Braid — " + name
}

// notificationBodyTaskFinished formats the body for an agent-turn-completed
// notification.
func notificationBodyTaskFinished(sessionTitle string) string {
	if sessionTitle == "" {
		return "Task finished"
	}
	return "Task finished: " + ansi.Truncate(sessionTitle, maxNotificationBodyLen, "…")
}

// notificationBodyTaskFailed formats the body for an agent-turn-errored
// notification.
func notificationBodyTaskFailed(errMessage string) string {
	errMessage = strings.TrimSpace(errMessage)
	if errMessage == "" {
		return "Task failed"
	}
	return "Task failed: " + ansi.Truncate(errMessage, maxNotificationBodyLen, "…")
}

// notificationBodyPermission formats the body for a permission-request
// notification.
func notificationBodyPermission(toolName string) string {
	return "Permission needed: " + toolName
}

// notificationBodyQuestions formats the body for a question-request
// notification.
func notificationBodyQuestions(count int) string {
	if count == 1 {
		return "Input needed: 1 question"
	}
	return fmt.Sprintf("Input needed: %d questions", count)
}

// selectNotificationBackend chooses the appropriate notification backend based
// on terminal capabilities, environment, and user configuration. This is a pure
// function that should be called once during initialization or when capabilities
// change.
func selectNotificationBackend(caps common.Capabilities, cfg *config.Config) notification.Backend {
	// Check for explicit user preference first.
	if cfg != nil && cfg.Options != nil && cfg.Options.Notifications != "" {
		switch cfg.Options.Notifications {
		case "native":
			if !notification.NativeSupported {
				slog.Debug("Native notifications unavailable on this platform; using OSC backend", "osc99_supported", caps.OSC99Notifications)
				return notification.NewOSCBackend(notification.Icon, caps.OSC99Notifications)
			}
			slog.Debug("Using native backend (user preference)")
			return notification.NewNativeBackend(notification.Icon)
		case "osc":
			slog.Debug("Using OSC backend (user preference)", "osc99_supported", caps.OSC99Notifications)
			return notification.NewOSCBackend(notification.Icon, caps.OSC99Notifications)
		case "bell":
			slog.Debug("Using bell backend (user preference)")
			return notification.NewBellBackend()
		case "disabled":
			slog.Debug("Notifications disabled (user preference)")
			return notification.NoopBackend{}
		case "auto":
			// Fall through to auto-detection below.
		default:
			slog.Warn("Unknown notification style, using auto", "style", cfg.Options.Notifications)
		}
	}

	// Auto-detect based on environment and capabilities.
	_, isSSH := caps.Env.LookupEnv("SSH_TTY")

	// SSH sessions use terminal-based notifications (OSC 99 or 777).
	if isSSH {
		slog.Debug("Selected OSCBackend for SSH session", "osc99_supported", caps.OSC99Notifications)
		return notification.NewOSCBackend(notification.Icon, caps.OSC99Notifications)
	}

	// Local sessions: prefer OSC on macOS because the native backend (beeep)
	// uses terminal-notifier or AppleScript, which is slow and doesn't display
	// icons properly. Also prefer OSC where native notifications are unavailable
	// (illumos/solaris). OSC 99 provides a polished experience with icon support.
	if runtime.GOOS == "darwin" || !notification.NativeSupported {
		slog.Debug("Selected OSCBackend for local session", "osc99_supported", caps.OSC99Notifications, "native_supported", notification.NativeSupported)
		return notification.NewOSCBackend(notification.Icon, caps.OSC99Notifications)
	}

	// Non-macOS local sessions use native OS notifications if focus events are supported.
	// Without focus events, we can't suppress notifications when focused, so
	// we disable them entirely to avoid spamming the user.
	if caps.ReportFocusEvents {
		slog.Debug("Selected NativeBackend for local session")
		return notification.NewNativeBackend(notification.Icon)
	}

	slog.Debug("Selected NoopBackend (focus events not supported)")
	return notification.NoopBackend{}
}

func (m *UI) updateNotificationBackend() {
	cfg := m.com.Config()
	m.notifyBackend = selectNotificationBackend(m.caps, cfg)
}

// shouldSendNotification returns true if notifications should be sent based on
// current state. Focus reporting must be supported, window must not be
// focused, and notifications must not be disabled in config.
func (m *UI) shouldSendNotification() bool {
	cfg := m.com.Config()
	if cfg != nil && cfg.Options != nil && cfg.Options.Notifications == "disabled" {
		return false
	}
	return m.caps.ReportFocusEvents && !m.notifyWindowFocused
}

// setState changes the UI state and focus.
func (m *UI) setState(state uiState, focus uiFocusState) {
	if state == uiLanding {
		// Always turn off compact mode when going to landing
		m.isCompact = false
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

// loadMCPrompts loads the MCP prompts asynchronously.
func (m *UI) loadMCPrompts() tea.Msg {
	prompts, err := m.com.Workspace.ListMCPPrompts(context.Background())
	if err != nil {
		slog.Error("Failed to load MCP prompts", "error", err)
	}
	if prompts == nil {
		// flag them as loaded even if there is none or an error
		prompts = []commands.MCPPrompt{}
	}
	return mcpPromptsLoadedMsg{Prompts: prompts}
}

// Update handles updates to the UI model.
func (m *UI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	// Update terminal capabilities
	m.caps.Update(msg)
	switch msg := msg.(type) {
	case tea.EnvMsg:
		// Is this Windows Terminal?
		if !m.sendProgressBar {
			m.sendProgressBar = slices.Contains(msg, "WT_SESSION")
		}
		cmds = append(cmds, common.QueryCmd(uv.Environ(msg)))
	case tea.ModeReportMsg:
		m.updateNotificationBackend()
	case uv.UnknownOscEvent:
		m.updateNotificationBackend()
	case tea.FocusMsg:
		m.notifyWindowFocused = true
	case tea.BlurMsg:
		m.notifyWindowFocused = false
	case pubsub.Event[notify.Notification]:
		if cmd := m.handleAgentNotification(msg.Payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case sessionsLoadedMsg:
		if cmd := m.applySessionsLoaded(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case busyStateMsg:
		cmds = append(cmds, m.applyBusyState(msg)...)
	case promptQueueMsg:
		cmds = append(cmds, m.applyPromptQueue(msg)...)
	case lspStatesMsg:
		if cmd := m.applyLSPStates(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case agentModelChangedMsg:
		// The coordinator model changed (selection, thinking, reasoning):
		// re-fetch the memoized ready/model state off-thread.
		m.invalidateBusyCaches()
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case agentRunSubmittedMsg:
		if m.sessionLoadExpectedID != "" && (msg.sessionID != m.sessionLoadExpectedID || msg.loadGeneration != m.sessionLoadGen) {
			break
		}
		// A prompt was just accepted (run started or enqueued): fetch the
		// authoritative busy/queue state to confirm the optimistic values
		// sendMessage wrote.
		m.invalidateBusyCaches()
		m.invalidatePromptQueue()
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.editor.pendingSendActive = false
		if len(m.editor.pendingSendQueue) > 0 {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
		}
	case loadSessionMsg:
		if msg.gen != m.sessionLoadGen || msg.sessionID != m.sessionLoadExpectedID {
			break
		}
		if msg.err != nil {
			// On error: discard pending sends and clear stale queue.
			m.editor.pendingSendQueue = nil
			m.editor.pendingSendGen = 0
			m.editor.pendingSendLoading = false
			cmds = append(cmds, util.ReportError(msg.err))
			break
		}
		if m.forceCompactMode {
			m.isCompact = true
		}
		m.setState(uiChat, m.focus)
		m.session = msg.session
		m.sidebar.offset = 0
		m.sessionFiles = msg.files
		// Session switch: the memoized busy state and queued prompts
		// belong to the previous session. Drop them and re-fetch
		// off-thread so the queue pill and esc behavior track the new
		// session instead of a stale one.
		m.invalidateBusyCaches()
		m.invalidatePromptQueue()
		m.wsCache.promptQueue = 0
		m.wsCache.promptQueueItems = nil
		m.wsCache.promptQueueCheckedAt = time.Time{}
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, m.startLSPs(msg.lspFilePaths()))
		if cmd := m.applySessionMessageItems(msg.items, msg.lastUserMessageTime); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.autoExpandTodosIfReasonable(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.syncPanelSpinner(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, m.reportCurrentSession(msg.sessionID))
		if hasInProgressTodo(m.session.Todos) {
			m.updateLayoutAndSize()
		}
		// Reload prompt history for the new session.
		m.editor.historyReset()
		cmds = append(cmds, m.loadPromptHistory())

		m.editor.pendingSendLoading = false
		m.editor.pendingSendActive = false
		if len(m.editor.pendingSendQueue) > 0 {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
		}
		m.updateLayoutAndSize()

	case requestSessionLoad:
		cmds = append(cmds, m.beginSessionLoad(msg.sessionID))

	case sessionFilesUpdatesMsg:
		m.sessionFiles = msg.sessionFiles
		var paths []string
		for _, f := range msg.sessionFiles {
			paths = append(paths, f.LatestVersion.Path)
		}
		cmds = append(cmds, m.startLSPs(paths))

	case sendMessageMsg:
		cmds = append(cmds, m.sendMessage(msg.Content, msg.Attachments...))

	case createSessionMsg:
		if !m.editor.pendingSendLoading || msg.generation != m.editor.pendingSendGen {
			break
		}
		expectedLoadGeneration := m.sessionLoadGen + 1
		for i := range m.editor.pendingSendQueue {
			if m.editor.pendingSendQueue[i].generation == msg.generation {
				m.editor.pendingSendQueue[i].sessionID = msg.session.ID
				m.editor.pendingSendQueue[i].loadGeneration = expectedLoadGeneration
			}
		}
		if m.forceCompactMode {
			m.isCompact = true
		}
		m.session = &msg.session
		m.setState(uiChat, m.focus)
		// Request loading the chat for the new session, then dispatch
		// sendMessage once the session is loaded.
		m.editor.pendingSendQueue = append([]sendQueueItem{{
			content:        msg.content,
			attachments:    msg.attachments,
			generation:     msg.generation,
			sessionID:      msg.session.ID,
			loadGeneration: expectedLoadGeneration,
		}}, m.editor.pendingSendQueue...)
		return m, m.requestSessionLoad(msg.session.ID)

	case userCommandsLoadedMsg:
		m.customCommands = msg.Commands
		dia := m.dialog.Dialog(dialog.CommandsID)
		if dia == nil {
			break
		}

		commands, ok := dia.(*dialog.Commands)
		if ok {
			commands.SetCustomCommands(m.customCommands)
		}

	case mcpStateChangedMsg:
		m.mcpStates = msg.states
		// Auto-open the MCP auth dialog if any servers need authentication.
		if cmd := m.openMCPAuthDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case mcpPromptsLoadedMsg:
		m.mcpPrompts = msg.Prompts
		dia := m.dialog.Dialog(dialog.CommandsID)
		if dia == nil {
			break
		}

		commands, ok := dia.(*dialog.Commands)
		if ok {
			commands.SetMCPPrompts(m.mcpPrompts)
		}

	case promptHistoryLoadedMsg:
		m.editor.promptHistory.messages = msg.messages
		m.editor.promptHistory.index = -1
		m.editor.promptHistory.draft = ""

	case closeDialogMsg:
		m.dialog.CloseFrontDialog()

	case pubsub.Event[session.Session]:
		if msg.Type == pubsub.DeletedEvent {
			if m.session != nil && m.session.ID == msg.Payload.ID {
				if cmd := m.newSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			break
		}
		if m.session != nil && msg.Payload.ID == m.session.ID {
			prevTodosLen := len(m.session.Todos)
			// mainRect.Dy() as of the last layout pass — main and panel
			// together reconstruct it, since generateLayout splits mainRect
			// into exactly those two rects. Only used here to detect
			// whether the panel's footprint changed, not as an actual
			// layout budget, so approximating off the last computed layout
			// (rather than recomputing mainRect from scratch) is fine.
			available := m.layout.main.Dy() + m.layout.panel.Dy()
			prevPanelHeight := m.sessionPanelHeight(available)
			m.session = &msg.Payload
			// syncPanelSpinner is idempotent and self-guarding — no need
			// to pre-compute the in-progress edge here.
			if cmd := m.syncPanelSpinner(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			// The session panel reserves vertical space that the chat area
			// must yield. Recompute the layout whenever that footprint
			// changes (todos appearing, the list growing, etc.) so the
			// panel renders on first paint rather than waiting for a
			// toggle. drawSessionPanel always paints from live state, so
			// no extra re-render is needed when the footprint is
			// unchanged (e.g. the in-progress spinner just ticks on the
			// next frame).
			if m.sessionPanelHeight(available) != prevPanelHeight {
				m.updateLayoutAndSize()
			}
			// While the panel is showing this session's todos, the chat
			// transcript's own todos tool call(s) render compact (header
			// only) instead of duplicating the full list; once every todo
			// is completed and the panel disappears, the transcript
			// becomes the permanent record again.
			m.chat.SetTodosCompact(hasIncompleteTodos(m.session.Todos))
			// A brand new list (0 -> N todos) always opens the panel,
			// unconditionally — distinct from autoExpandTodosIfReasonable
			// below, which is a gentler one-shot-per-session, tall-enough-
			// terminal nicety for the "resumed a session that already had
			// an active list" case. This only fires on the transition
			// itself: a later update to the same list (items added,
			// statuses changed) must respect a user's manual collapse.
			if prevTodosLen == 0 && len(m.session.Todos) > 0 {
				m.panel.expanded = true
			}
			m.autoExpandTodosIfReasonable()
		} else {
			// Not the current session — it may be a running delegation's
			// child session, updated as its own turns complete. Surface
			// its running token count on the parent's status line.
			m.handleChildSessionUpdate(msg.Payload)
		}
	case pubsub.Event[message.Message]:
		// Check if this is a child session message for an agent tool.
		if m.session == nil {
			break
		}
		if msg.Payload.SessionID != m.session.ID {
			// This might be a child session message from an agent tool.
			if cmd := m.handleChildSessionMessage(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			break
		}
		switch msg.Type {
		case pubsub.CreatedEvent:
			cmds = append(cmds, m.appendSessionMessage(msg.Payload))
			// A new message is a run boundary — a user prompt starting
			// a turn or the agent replying/dequeueing. Drop the
			// memoized busy state and re-fetch it and the queue
			// off-thread. Per-chunk UpdatedEvents deliberately do NOT
			// trigger this: during streaming that would put workspace
			// probes on every token.
			m.invalidateBusyCaches()
			m.invalidatePromptQueue()
			if cmd := m.dispatchBusyRefresh(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		case pubsub.UpdatedEvent:
			cmds = append(cmds, m.updateSessionMessage(msg.Payload))
		case pubsub.DeletedEvent:
			m.chat.RemoveMessage(msg.Payload.ID)
		}
		// Reconcile the spinner with the new message's implications
		// (a turn starting or ending changes what's live).
		if cmd := m.syncPanelSpinner(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[history.File]:
		cmds = append(cmds, m.handleFileEvent(msg.Payload))
	case pubsub.Event[app.LSPEvent]:
		// Refresh the memoized LSP state off-thread: LSPGetStates is a
		// synchronous HTTP round-trip in client/server mode and diagnostics
		// events can arrive per edited file.
		if cmd := m.requestLSPRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[workspace.LSPEvent]:
		if cmd := m.requestLSPRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[skills.Event]:
		m.skillStates = msg.Payload.States
	case pubsub.Event[mcp.Event]:
		switch msg.Payload.Type {
		case mcp.EventStateChanged:
			return m, tea.Batch(
				m.handleStateChanged(),
				m.loadMCPrompts,
			)
		case mcp.EventPromptsListChanged:
			return m, handleMCPPromptsEvent(m.com.Workspace, msg.Payload.Name)
		case mcp.EventToolsListChanged:
			return m, handleMCPToolsEvent(m.com.Workspace, msg.Payload.Name)
		case mcp.EventResourcesListChanged:
			return m, handleMCPResourcesEvent(m.com.Workspace, msg.Payload.Name)
		}
	case pubsub.Event[permission.PermissionRequest]:
		if cmd := m.openPermissionsDialog(msg.Payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.sendNotification(notification.Notification{
			Title:   notificationTitle(m.com.Workspace.WorkingDir()),
			Message: notificationBodyPermission(msg.Payload.ToolName),
		}); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[permission.PermissionNotification]:
		m.handlePermissionNotification(msg.Payload)
	case pubsub.Event[question.Request]:
		m.openBatchFormDialog(msg.Payload)
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.sendNotification(notification.Notification{
			Title:   notificationTitle(m.com.Workspace.WorkingDir()),
			Message: notificationBodyQuestions(len(msg.Payload.Questions)),
		}); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[question.Notification]:
		m.handleQuestionNotification(msg.Payload)
	case providerConfiguredResult:
		if msg.generation != m.modelOperationGeneration {
			break
		}
		if msg.Err != nil {
			m.modelOperationLoading = false
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		cmds = append(cmds, m.initAgentAndReportModel(true, msg.Model, msg.generation))

	case modelSelectResult:
		if msg.generation != m.modelOperationGeneration {
			break
		}
		if msg.Err != nil {
			m.modelOperationLoading = false
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		cmds = append(cmds, m.initAgentAndReportModel(msg.Onboarding, msg.Model, msg.generation))

	case agentModelInitializedMsg:
		if msg.generation != m.modelOperationGeneration {
			break
		}
		m.modelOperationLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		if msg.Onboarding {
			m.setState(uiLanding, uiFocusEditor)
		}
		modelName := msg.Model.Model
		if cfg := m.com.Config(); cfg != nil {
			if selected := cfg.GetModel(msg.Model.Provider, msg.Model.Model); selected != nil && selected.Name != "" {
				modelName = selected.Name
			}
		}
		cmds = append(cmds, util.ReportInfo(fmt.Sprintf("Model changed to %s", modelName)), func() tea.Msg { return agentModelChangedMsg{} })

	case modelSettingUpdatedMsg:
		if msg.generation != m.modelOperationGeneration {
			break
		}
		m.modelOperationLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
		} else {
			cmds = append(cmds, util.ReportInfo(msg.Info))
		}

	case transparentToggledMsg:
		if msg.generation != m.transparentGeneration {
			break
		}
		m.transparentLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		m.isTransparent = msg.Enabled
		m.dialog.CloseDialog(dialog.CommandsID)

	case compactModeToggledMsg:
		if msg.generation != m.compactModeGeneration {
			break
		}
		m.compactModeLoading = false
		if msg.Err == nil {
			m.forceCompactMode = msg.Enabled
			m.isCompact = msg.Enabled
			m.updateLayoutAndSize()
			m.dialog.CloseDialog(dialog.CommandsID)
		} else {
			cmds = append(cmds, util.ReportError(msg.Err))
		}

	case notificationStyleSetMsg:
		if msg.generation != m.notificationGeneration {
			break
		}
		m.notificationLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		m.updateNotificationBackend()
		m.dialog.CloseDialog(dialog.NotificationsID)
		cmds = append(cmds, util.ReportInfo("Notifications set to: "+msg.Style))

	case permissionResponseMsg:
		if msg.generation != m.permissionGeneration || msg.Permission != m.permissionID {
			break
		}
		m.permissionLoading = false
		if !msg.Accepted {
			cmds = append(cmds, util.ReportError(errors.New("permission response was not accepted")))
			break
		}
		m.dialog.CloseDialog(dialog.PermissionsID)

	case yoloToggledMsg:
		if msg.generation != m.yoloGeneration {
			break
		}
		m.yoloLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		m.wsCache.yoloCache.set(msg.Enabled)
		m.wsCache.busyFetchGen++
		m.setEditorPrompt(msg.Enabled)
		status := "disabled"
		if msg.Enabled {
			status = "enabled"
		}
		cmds = append(cmds, util.ReportInfo("Yolo mode "+status))

	case notificationSentMsg:
		m.updateNotificationBackend()

	case importCopilotResult:
		if msg.generation != m.modelOperationGeneration {
			break
		}
		// ImportCopilot completed (successfully or not). Now check
		// whether the provider is actually configured.
		cfg := m.com.Config()
		ws := m.com.Workspace
		isConfigured := func() bool {
			_, ok := cfg.Providers.Get(msg.providerID)
			return ok
		}
		if !isConfigured() {
			m.modelOperationLoading = false
			m.dialog.CloseDialog(dialog.ModelsID)
			provider := catwalk.Provider{ID: catwalk.InferenceProvider(msg.providerID)}
			if cmd := m.openAuthenticationDialog(provider, msg.model, ""); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return m, tea.Batch(cmds...)
		}
		// Provider is configured after import: proceed to UpdatePreferredModel.
		capturedModel := msg.model
		generation := msg.generation
		cmds = append(cmds, func() tea.Msg {
			if err := ws.UpdatePreferredModel(config.ScopeGlobal, capturedModel); err != nil {
				return modelSelectResult{Err: err, generation: generation}
			}
			return modelSelectResult{Onboarding: msg.isOnboarding, Model: capturedModel, generation: generation}
		})
		return m, tea.Batch(cmds...)

	case sendMessageErrorMsg:
		if !msg.creating && m.sessionLoadExpectedID != "" && (msg.sessionID != m.sessionLoadExpectedID || msg.loadGeneration != m.sessionLoadGen) {
			break
		}
		m.editor.pendingSendActive = false
		if msg.creating && msg.generation == m.editor.pendingSendGen {
			m.editor.pendingSendLoading = false
			m.editor.pendingSendQueue = nil
		}
		cmds = append(cmds, util.ReportError(msg.Err))
		m.wsCache.agentBusyCache.set(false)
		if !msg.creating && len(m.editor.pendingSendQueue) > 0 {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
		}

	case sendPendingQueueMsg:
		if m.editor.pendingSendActive || len(m.editor.pendingSendQueue) == 0 || m.session == nil {
			break
		}
		item := m.editor.pendingSendQueue[0]
		m.editor.pendingSendQueue = m.editor.pendingSendQueue[1:]
		if item.sessionID != m.session.ID || item.loadGeneration != m.sessionLoadGen {
			if len(m.editor.pendingSendQueue) > 0 {
				cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
			}
			break
		}
		m.editor.pendingSendActive = true
		if item.bang {
			cmds = append(cmds, m.runShellCommandInternal(item.content, item.isFirstMessage))
		} else {
			cmds = append(cmds, m.sendMessageNow(item.content, item.attachments...))
		}

	case bangSessionCreatedMsg:
		if !m.editor.pendingSendLoading || msg.generation != m.editor.pendingSendGen {
			break
		}
		expectedLoadGeneration := m.sessionLoadGen + 1
		for i := range m.editor.pendingSendQueue {
			if m.editor.pendingSendQueue[i].generation == msg.generation {
				m.editor.pendingSendQueue[i].sessionID = msg.session.ID
				m.editor.pendingSendQueue[i].loadGeneration = expectedLoadGeneration
			}
		}
		m.editor.pendingSendQueue = append([]sendQueueItem{{
			content:        msg.command,
			generation:     msg.generation,
			sessionID:      msg.session.ID,
			loadGeneration: expectedLoadGeneration,
			bang:           true,
			isFirstMessage: msg.isFirstMessage,
		}}, m.editor.pendingSendQueue...)
		m.session = &msg.session
		m.setState(uiChat, m.focus)
		cmds = append(cmds, m.requestSessionLoad(msg.session.ID))

	case cancelTimerExpiredMsg:
		m.isCanceling = false
	case tea.TerminalVersionMsg:
		termVersion := strings.ToLower(msg.Name)
		// Only enable progress bar for the following terminals.
		if !m.sendProgressBar {
			m.sendProgressBar = xstrings.ContainsAnyOf(termVersion, "ghostty", "iterm2", "rio")
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Suppress the chat's full-height scan during the resize so a drag
		// only reflows visible items; it settles (and recomputes) shortly
		// after the last resize event.
		if m.state == uiChat {
			cmds = append(cmds, m.chat.BeginResize())
		}
		m.updateLayoutAndSize()
		if m.state == uiChat && m.chat.Follow() {
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case tea.KeyboardEnhancementsMsg:
		m.keyenh = msg
		if msg.SupportsKeyDisambiguation() {
			if slices.Contains(m.keyMap.Models.Keys(), "ctrl+m") {
				m.keyMap.Models.SetHelp("ctrl+m", "models")
			} else if slices.Contains(m.keyMap.Models.Keys(), "super+m") {
				m.keyMap.Models.SetHelp("super+m", "models")
			}
			if slices.Contains(m.keyMap.Editor.Newline.Keys(), "shift+enter") {
				m.keyMap.Editor.Newline.SetHelp("shift+enter", "newline")
			}
		}
	case copyChatHighlightMsg:
		cmds = append(cmds, m.copyChatHighlight())
	case DelayedClickMsg:
		// Handle delayed single-click action (e.g., expansion, or
		// navigating into a clicked child-session delegation). messageID
		// and toolCallID come from the clicked item directly, not
		// m.chat.SelectedNestedToolContainer — a mouse click doesn't move
		// the keyboard-driven selection that reads (see HandleMouseDown).
		if _, openContainer, messageID, toolCallID := m.chat.HandleDelayedClick(msg); openContainer {
			cmds = append(cmds, m.enterChildSession(messageID, toolCallID))
		}
	case tea.MouseClickMsg:
		// Pass mouse events to dialogs first if any are open. Route through
		// handleDialogMsg (not a bare dialog.Update) so a click's resulting
		// Action — e.g. clicking a permissions dialog button — is actually
		// processed the same way a keyboard-driven selection is.
		if m.dialog.HasDialogs() {
			if cmd := m.handleDialogMsg(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return m, tea.Batch(cmds...)
		}

		// Route clicks to inline editors that support mouse interaction.
		if m.activeInline != nil {
			if clickable, ok := m.activeInline.(dialog.MouseClickableEditor); ok {
				if done, handled := clickable.HandleMouseClick(msg.X, msg.Y); handled {
					if done {
						m.activeInline = nil
						m.editor.textarea.Focus()
						m.updateLayoutAndSize()
					}
					return m, tea.Batch(cmds...)
				}
			}
		}

		// A click anywhere on the header while threads are active opens the
		// threads dashboard — the badge rendered there (see header.go's
		// renderHeaderDetails) is the only visible hint threads are running
		// while on the main screen, so it doubles as a button.
		if msg.Button == tea.MouseLeft && m.threadIndicator.count > 0 && image.Pt(msg.X, msg.Y).In(m.layout.header) {
			cmds = append(cmds, util.CmdHandler(showThreadsDashboardMsg{}))
			return m, tea.Batch(cmds...)
		}

		// A click anywhere on the child-session panel (see
		// drawChildSessionPanel, which occupies the editor area in place of
		// the textarea while a child session is being viewed) exits the
		// child session. Mouse clicks never change m.focus (see
		// uiFocusState) — this is the one dedicated, deliberate transition
		// a click is allowed to trigger.
		if msg.Button == tea.MouseLeft && m.viewingChildSession() && image.Pt(msg.X, msg.Y).In(m.layout.editor) {
			if cmd := m.exitChildSession(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return m, tea.Batch(cmds...)
		}

		// A click on the session panel's todos header row toggles its
		// expand state; a click on a rendered thread block drills into
		// that thread's own session — the same transition Enter takes on
		// the threads dashboard (see Root.attachThreadCmd), not
		// enterChildSession/navStack, which point at the wrong workspace.
		//
		// Hit-test rects are recomputed here from m.layout.panel +
		// m.sessionPanelPlan (via sessionPanelRowLayout), NOT read from
		// m.panelTodosHeaderRect/m.panelThreadRects: those are only
		// populated as a side effect of drawSessionPanel, which runs inside
		// Draw/View. A click can be delivered by Update before View has
		// ever painted the current layout (e.g. right after
		// updateLayoutAndSize runs synchronously inside Update in response
		// to a session/todos event), which would leave the cached rects
		// stale or zero and silently swallow the click.
		if msg.Button == tea.MouseLeft && m.state == uiChat && m.hasSession() {
			pt := image.Pt(msg.X, msg.Y)
			plan := m.sessionPanelPlan(m.layout.panel.Dy())
			threadBlockRects, delegationBlockRects, todosHeaderRect, _ := sessionPanelRowLayout(m.layout.panel, plan)
			if pt.In(todosHeaderRect) {
				if cmd := m.toggleTodosExpanded(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return m, tea.Batch(cmds...)
			}
			for i, rect := range threadBlockRects {
				if !pt.In(rect) {
					continue
				}
				th := plan.threads[i]
				cmds = append(cmds, util.CmdHandler(enterThreadMsg{id: th.ID, sessionID: th.SessionID}))
				return m, tea.Batch(cmds...)
			}
			// A click on a delegation block drills into its child session —
			// real navStack-based drill-in via enterChildSession, unlike
			// threads (which need AttachThread): a delegation's session
			// already lives in this same workspace/DB.
			for i, rect := range delegationBlockRects {
				if !pt.In(rect) {
					continue
				}
				d := plan.delegations[i]
				if cmd := m.enterChildSession(d.messageID, d.toolCallID); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return m, tea.Batch(cmds...)
			}
		}

		// Check if the click landed on an attachment's remove button.
		// The attachment chips are rendered on the first row of the
		// editor layout area, above the textarea.
		if m.activeInline == nil && msg.Button == uv.MouseLeft && len(m.editor.attachments.List()) > 0 && msg.Y == m.layout.editor.Min.Y {
			relX := msg.X - m.layout.editor.Min.X
			if m.editor.attachments.HandleClick(relX) {
				return m, tea.Batch(cmds...)
			}
		}

		switch m.state {
		case uiChat:
			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.layout.main.Min.X
			y -= m.layout.main.Min.Y
			if !image.Pt(msg.X, msg.Y).In(m.layout.sidebar) {
				if handled, cmd := m.chat.HandleScrollbarMouseDown(x, y); handled {
					if cmd != nil {
						cmds = append(cmds, cmd)
					}
				} else if handled, cmd := m.chat.HandleMouseDown(x, y); handled {
					m.lastClickTime = time.Now()
					if cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
		}

	case tea.MouseMotionMsg:
		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return m, tea.Batch(cmds...)
		}

		// Hover feedback for the child-session panel's "back" button.
		if m.viewingChildSession() {
			m.childPanelHover = image.Pt(msg.X, msg.Y).In(m.layout.editor)
		}

		if m.activeInline == nil && len(m.editor.attachments.List()) > 0 && msg.Y == m.layout.editor.Min.Y {
			m.editor.attachments.SetHover(msg.X - m.layout.editor.Min.X)
		} else {
			m.editor.attachments.SetHover(-1)
		}

		// Hover feedback for the session panel's todos header, thread
		// blocks, and delegation blocks, mirroring the child-session
		// panel's hover pattern above.
		if m.state == uiChat {
			pt := image.Pt(msg.X, msg.Y)
			plan := m.sessionPanelPlan(m.layout.panel.Dy())
			threadRects, delegationRects, todosHeaderRect, _ := sessionPanelRowLayout(m.layout.panel, plan)
			m.panelTodosHover = pt.In(todosHeaderRect)
			m.hoveredPanelThread = -1
			for i, rect := range threadRects {
				if pt.In(rect) {
					m.hoveredPanelThread = i
					break
				}
			}
			m.hoveredPanelDelegation = -1
			for i, rect := range delegationRects {
				if pt.In(rect) {
					m.hoveredPanelDelegation = i
					break
				}
			}
		}

		// Track hover position for inline editors.
		if m.activeInline != nil {
			if m.hoverX != msg.X || m.hoverY != msg.Y {
				m.hoverX = msg.X
				m.hoverY = msg.Y
				if clickable, ok := m.activeInline.(dialog.MouseClickableEditor); ok {
					clickable.SetHover(msg.X, msg.Y)
				}
			}
		}

		switch m.state {
		case uiChat:
			// Skip chat edge-scrolling when an inline editor is
			// active to prevent accidental scrolling while hovering
			// over question forms or other inline components.
			if m.activeInline != nil && m.focus == uiFocusEditor {
				break
			}

			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.layout.main.Min.X
			y -= m.layout.main.Min.Y

			// An active scrollbar drag takes over the whole gesture: it
			// tracks the cursor directly and must not also trigger the
			// text-selection edge-scroll below (the two would fight over
			// the offset).
			if handled, cmd := m.chat.HandleScrollbarMouseDrag(x, y); handled {
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
				break
			}

			if msg.Y <= 0 {
				if cmd := m.chat.ScrollByAndAnimate(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectPrev()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			} else if msg.Y >= m.chat.Height()-1 {
				if cmd := m.chat.ScrollByAndAnimate(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectNext()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}

			m.chat.HandleMouseDrag(x, y)
			m.chat.HandleMouseHover(x, y)
			m.chat.ScrollbarHoverAt(x, y)
		}

	case tea.MouseReleaseMsg:
		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return m, tea.Batch(cmds...)
		}

		switch m.state {
		case uiChat:
			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.layout.main.Min.X
			y -= m.layout.main.Min.Y
			if m.chat.HandleScrollbarMouseUp() {
				// Scrollbar drag ended; nothing else to do.
			} else if m.chat.HandleMouseUp(x, y) && m.chat.HasHighlight() {
				cmds = append(cmds, tea.Tick(doubleClickThreshold, func(t time.Time) tea.Msg {
					if time.Since(m.lastClickTime) >= doubleClickThreshold {
						return copyChatHighlightMsg{}
					}
					return nil
				}))
			}
		}
	case common.CoalescedWheelMsg:
		// Route wheel events to active inline editor only when the
		// mouse is over the editor area, so scrolling over the chat
		// still scrolls the chat.
		if m.activeInline != nil && image.Pt(msg.Mouse.X, msg.Mouse.Y).In(m.layout.editor) {
			if we, ok := m.activeInline.(common.WheelScrollable); ok {
				we.HandleWheel(msg.DeltaX, msg.DeltaY)
				return m, tea.Batch(cmds...)
			}
		}

		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return m, tea.Batch(cmds...)
		}

		// Otherwise handle mouse wheel for chat. Use the coalesced delta
		// directly as the line count. Terminals like Ghostty send DeltaY=3
		// per physical wheel tick (matching their native scrollback), while
		// others send DeltaY=1.
		switch m.state {
		case uiChat:
			// When the mouse is hovering the sidebar, route wheel events to
			// sidebar scrolling. Focus never enters the sidebar (see
			// uiFocusState), so this is purely a hover check.
			if m.sidebar.scrollable && image.Pt(msg.Mouse.X, msg.Mouse.Y).In(m.layout.sidebar) {
				lines := int(msg.DeltaY)
				if lines != 0 {
					seq := m.sidebar.scrollByWheel(lines)
					cmds = append(cmds, sidebarScrollbarHideCmd(seq))
				}
				break
			}
			// When the mouse is hovering the session panel's scrollable
			// todos rows, the wheel scrolls that section's own offset
			// instead of the chat — recomputed fresh from
			// sessionPanelRowLayout (not the m.panelTodosListRect cache),
			// same rationale as the click hit-test above: a wheel event
			// can arrive before drawSessionPanel has painted the current
			// layout.
			if m.hasSession() {
				plan := m.sessionPanelPlan(m.layout.panel.Dy())
				if plan.todosScrollable {
					_, _, _, todosListRect := sessionPanelRowLayout(m.layout.panel, plan)
					if image.Pt(msg.Mouse.X, msg.Mouse.Y).In(todosListRect) {
						lines := int(msg.DeltaY)
						if lines != 0 {
							m.panelTodosScrollOffset = clampPanelTodosScrollOffset(m.panelTodosScrollOffset+lines, plan)
						}
						break
					}
				}
			}
			if msg.DeltaX != 0 {
				m.chat.ScrollSelectedShellHorizontal(int(msg.DeltaX))
			}
			lines := int(msg.DeltaY)
			if lines == 0 {
				break
			}
			if cmd := m.chat.ScrollByAndAnimate(lines); cmd != nil {
				cmds = append(cmds, cmd)
			}
			if !m.chat.SelectedItemInView() {
				if lines < 0 {
					m.chat.SelectPrev()
				} else if m.chat.AtBottom() {
					m.chat.SelectLast()
				} else {
					m.chat.SelectNext()
				}
				if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	case anim.StepMsg:
		if m.state == uiChat {
			if cmd := m.chat.Animate(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.chat.Follow() {
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	case scrollbarHideMsg:
		if m.state == uiChat {
			m.chat.HideScrollbar(msg.seq)
		}
	case chatWarmMsg:
		// A resize has settled; warm the message cache one batch at a time
		// so the scrollbar recompute never blocks the UI thread.
		if m.state == uiChat {
			cmd, done := m.chat.WarmStep(msg.seq)
			if cmd != nil {
				cmds = append(cmds, cmd)
			} else if done {
				// Heights are cached now, so the final layout pass (scrollbar
				// reservation) is cheap.
				m.updateLayoutAndSize()
			}
		}
	case sidebarScrollbarHideMsg:
		if msg.seq == m.sidebar.scrollbarSeq {
			m.sidebar.hideScrollbar()
		}
	case spinner.TickMsg:
		if m.dialog.HasDialogs() {
			// route to dialog
			if cmd := m.handleDialogMsg(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		// Stop the tick loop when nothing live is left (or the chat screen
		// isn't showing); syncPanelSpinner re-arms it on the next relevant
		// event. Letting the loop die and be restarted beats ticking
		// forever behind an idle screen.
		if m.panelIsSpinning && (m.state != uiChat || !m.panelSpinnerWanted()) {
			m.panelIsSpinning = false
		}
		if m.panelIsSpinning {
			var cmd tea.Cmd
			m.panelSpinner, cmd = m.panelSpinner.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
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
	case openEditorMsg:
		prevHeight := m.editor.textarea.Height()
		m.editor.textarea.SetValue(msg.Text)
		m.editor.textarea.MoveToEnd()
		m.syncBangModeFromTextarea()
		cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))
	case shellStreamMsg:
		if item := m.chat.MessageItem(msg.PendingID); item != nil {
			if shellItem, ok := item.(*chat.ShellItem); ok {
				shellItem.AppendOutput(msg.Chunk)
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		// Continue draining the stream channel.
		if msg.streamCh != nil {
			ch := msg.streamCh
			pid := msg.PendingID
			cmds = append(cmds, func() tea.Msg {
				chunk, ok := <-ch
				if !ok {
					return nil
				}
				return shellStreamMsg{PendingID: pid, Chunk: chunk, streamCh: ch}
			})
		}
	case shellResultMsg:
		if (m.sessionLoadExpectedID != "" && msg.sessionID != m.sessionLoadExpectedID) || msg.generation != m.sessionLoadGen {
			break
		}
		m.editor.pendingSendActive = false
		// Clear the bang cancel func — command is done.
		if m.editor.bangCancel != nil {
			m.editor.bangCancel()
			m.editor.bangCancel = nil
		}
		// Complete the pending shell item if it exists, otherwise create a new one.
		completed := false
		if msg.PendingID != "" {
			if item := m.chat.MessageItem(msg.PendingID); item != nil {
				if shellItem, ok := item.(*chat.ShellItem); ok {
					shellItem.Complete(msg.Output, msg.ExitCode)
					if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
					completed = true
				}
			}
		}
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(fmt.Errorf("shell command failed: %w", msg.Err)))
		}
		if !completed {
			item := chat.NewShellItem(m.com.Styles, msg.Command, msg.Output, msg.ExitCode)
			m.chat.AppendMessages(item)
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		cmds = append(cmds, m.loadPromptHistory())
		if len(m.editor.pendingSendQueue) > 0 {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
		}
	case util.InfoMsg:
		if msg.Type == util.InfoTypeError {
			slog.Error("Error reported", "error", msg.Msg)
		}
		m.status.SetInfoMsg(msg)
		ttl := msg.TTL
		if ttl <= 0 {
			ttl = DefaultStatusTTL
		}
		cmds = append(cmds, clearInfoMsgCmd(ttl))
	case pubsub.Event[proto.ServerNotice]:
		// Server-originated notices (e.g. a client/server version
		// mismatch) arrive as the transport-neutral proto.ServerNotice
		// so backend code doesn't need to depend on internal/ui; convert
		// to util.InfoMsg here at the boundary.
		info := util.InfoMsg{
			Type: serverNoticeLevelToInfoType(msg.Payload.Level),
			Msg:  msg.Payload.Message,
		}
		m.status.SetInfoMsg(info)
		ttl := info.TTL
		if ttl <= 0 {
			ttl = DefaultStatusTTL
		}
		cmds = append(cmds, clearInfoMsgCmd(ttl))
	case pubsub.Event[proto.Thread]:
		// Root fans this to both screens (see root.go); the main screen
		// only cares about keeping the header badge's count current.
		m.threadIndicator.applyEvent(msg)
		m.threadsDock.applyThreadEvent(msg)
		cmds = append(cmds, m.threadViewsRefreshCmds()...)
		// A thread's edge transition into a terminal status (merged,
		// failed, ...) gets a toast — see thread_completion.go for why a
		// toast rather than a persisted chat entry.
		if cmd := m.notifyThreadCompletion(msg.Payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
		// A thread starting or finishing changes whether the panel has
		// live work to animate.
		if cmd := m.syncPanelSpinner(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case threadIndicatorLoadedMsg:
		if cmd := m.threadIndicator.applyLoaded(m.com, msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case threadsDockLoadedMsg:
		if cmd := m.threadsDock.applyThreadsDockLoaded(m.com, msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
		// The freshly listed threads may introduce (or retire) live work.
		if cmd := m.syncPanelSpinner(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case threadDockActivityLoadedMsg:
		m.threadsDock.applyThreadActivityLoaded(msg)
	case app.UpdateAvailableMsg:
		text := fmt.Sprintf("Braid update available: v%s → v%s.", msg.CurrentVersion, msg.LatestVersion)
		if msg.IsDevelopment {
			text = fmt.Sprintf("This is a development version of Braid. The latest version is v%s.", msg.LatestVersion)
		}
		ttl := 10 * time.Second
		m.status.SetInfoMsg(util.InfoMsg{
			Type: util.InfoTypeUpdate,
			Msg:  text,
			TTL:  ttl,
		})
		cmds = append(cmds, clearInfoMsgCmd(ttl))
	case workspace.ConnectionEvent:
		cmds = append(cmds, m.handleConnectionEvent(msg)...)
	case util.ClearStatusMsg:
		m.status.ClearInfoMsg()
	case completions.CompletionItemsLoadedMsg:
		if m.editor.completionsOpen {
			m.editor.completions.SetItems(msg.Files, msg.Resources)
		}
	case uv.KittyGraphicsEvent:
		if !bytes.HasPrefix(msg.Payload, []byte("OK")) {
			slog.Warn("Unexpected Kitty graphics response",
				"response", string(msg.Payload),
				"options", msg.Options)
		}
	case dialog.ActionMCPAuthStarted:
		cmds = append(cmds, m.authenticateMCP(msg.Ctx, msg.Name))
	case dialog.ActionMCPAuthComplete, dialog.ActionMCPAuthErrored:
		if m.dialog.HasDialogs() {
			if cmd := m.handleDialogMsg(msg); cmd != nil {
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

// serverNoticeLevelToInfoType maps the transport-neutral
// proto.ServerNoticeLevel to the UI's own status-line severity type.
func serverNoticeLevelToInfoType(level proto.ServerNoticeLevel) util.InfoType {
	switch level {
	case proto.ServerNoticeLevelWarn:
		return util.InfoTypeWarn
	case proto.ServerNoticeLevelError:
		return util.InfoTypeError
	default:
		return util.InfoTypeInfo
	}
}

// setSessionMessages sets the messages for the current session in the chat
func (m *UI) setSessionMessages(msgs []message.Message) tea.Cmd {
	items, lastUserMessageTime := m.sessionMessageItems(msgs)
	m.loadNestedToolCalls(items)
	return m.applySessionMessageItems(items, lastUserMessageTime)
}

func (m *UI) sessionMessageItems(msgs []message.Message) ([]chat.MessageItem, int64) {
	return sessionMessageItems(m.com.Styles, m.com.Config(), msgs)
}

func sessionMessageItems(sty *styles.Styles, cfg *config.Config, msgs []message.Message) ([]chat.MessageItem, int64) {
	msgPtrs := make([]*message.Message, len(msgs))
	for i := range msgs {
		msgPtrs[i] = &msgs[i]
	}
	toolResultMap := chat.BuildToolResultMap(msgPtrs)
	var lastUserMessageTime int64
	if len(msgPtrs) > 0 {
		lastUserMessageTime = msgPtrs[0].CreatedAt
	}
	items := make([]chat.MessageItem, 0, len(msgs)*2)
	for _, msg := range msgPtrs {
		if msg.Role == message.User {
			lastUserMessageTime = msg.CreatedAt
		}
		items = append(items, chat.ExtractMessageItems(sty, msg, toolResultMap, cfg)...)
		if msg.Role == message.Assistant && msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn {
			items = append(items, chat.NewAssistantInfoItem(sty, msg, cfg, time.Unix(lastUserMessageTime, 0)))
		}
	}
	return items, lastUserMessageTime
}

func (m *UI) applySessionMessageItems(items []chat.MessageItem, lastUserMessageTime int64) tea.Cmd {
	var cmds []tea.Cmd
	m.lastUserMessageTime = lastUserMessageTime
	// If the user switches between sessions while the agent is working we
	// want to make sure the animations are shown. Gate on the agent actually
	// being busy: a session that was killed mid-generation can persist an
	// assistant message with no Finish part, which still reports isSpinning()
	// even though nothing is running. Starting animations for it here would
	// leave a ghost "working" spinner (and a second one alongside any tool
	// spinner) after the session is reloaded.
	if m.isAgentBusy() {
		for _, item := range items {
			if animatable, ok := item.(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	}

	if cmd := m.chat.SetMessages(items...); cmd != nil {
		cmds = append(cmds, cmd)
	}
	// New items just replaced the whole list — sync their todos compact
	// state to whether the panel is currently showing this session's
	// todos, same as the pubsub session-update handler does for
	// already-loaded items.
	if m.hasSession() {
		m.chat.SetTodosCompact(hasIncompleteTodos(m.session.Todos))
	}
	if cmd := m.chat.RestartPausedVisibleAnimations(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	m.chat.SelectLast()
	return tea.Sequence(cmds...)
}

// handleConnectionEvent reports the health of the client-server link and,
// once it recovers, reloads the open session. A reload is always needed
// after a degraded episode: events published while the stream was down are
// gone, and if the workspace itself was re-created any run died with it.
func (m *UI) handleConnectionEvent(msg workspace.ConnectionEvent) []tea.Cmd {
	info := util.InfoMsg{
		Type: util.InfoTypeWarn,
		Msg:  "Lost connection to the Braid server — reconnecting…",
		TTL:  30 * time.Second,
	}
	switch msg.State {
	case workspace.ConnectionDegraded:
		slog.Warn("Server connection degraded", "error", msg.Err, "stuck", msg.Stuck)
		if msg.Stuck {
			info.Type = util.InfoTypeError
			info.Msg = "Can't restore the connection to the Braid server. Restart Braid to recover."
			info.TTL = time.Minute
		}
	case workspace.ConnectionRecovered:
		info = util.InfoMsg{
			Type: util.InfoTypeSuccess,
			Msg:  "Reconnected to the Braid server.",
			TTL:  DefaultStatusTTL,
		}
	}
	m.status.SetInfoMsg(info)
	cmds := []tea.Cmd{clearInfoMsgCmd(info.TTL)}
	if msg.State == workspace.ConnectionRecovered && m.session != nil {
		cmds = append(cmds, m.requestSessionLoad(m.session.ID))
	}
	return cmds
}

func (m *UI) loadNestedToolCalls(items []chat.MessageItem) {
	if m.session != nil {
		_ = loadNestedToolCalls(context.Background(), m.com.Workspace, m.com.Styles, m.com.Config(), m.session.ID, m.sessionLoadGen, items)
	}
}

type childLoad struct {
	sessionID string
	container chat.NestedToolContainer
	tools     []chat.ToolMessageItem
}

func loadNestedToolCalls(ctx context.Context, ws workspace.Workspace, sty *styles.Styles, cfg *config.Config, rootSessionID string, generation uint64, items []chat.MessageItem) error {
	var children []childLoad
	for _, item := range items {
		nestedContainer, ok := item.(chat.NestedToolContainer)
		if !ok {
			continue
		}
		toolItem, ok := item.(chat.ToolMessageItem)
		if !ok {
			continue
		}
		tc := toolItem.ToolCall()
		children = append(children, childLoad{
			sessionID: ws.CreateAgentToolSessionID(toolItem.MessageID(), tc.ID),
			container: nestedContainer,
		})
	}

	if len(children) == 0 {
		return nil
	}

	sessionIDs := make([]string, len(children))
	for i, c := range children {
		sessionIDs[i] = c.sessionID
	}

	nestedMsgsMap, err := ws.ListMessagesBySessionIDs(ctx, rootSessionID, generation, sessionIDs)
	if err != nil {
		return err
	}

	var deeperItems []chat.MessageItem
	for i := range children {
		c := &children[i]
		nestedMsgs, ok := nestedMsgsMap[c.sessionID]
		if !ok || len(nestedMsgs) == 0 {
			continue
		}

		nestedMsgPtrs := make([]*message.Message, len(nestedMsgs))
		for i := range nestedMsgs {
			nestedMsgPtrs[i] = &nestedMsgs[i]
		}
		nestedToolResultMap := chat.BuildToolResultMap(nestedMsgPtrs)

		for _, nestedMsg := range nestedMsgPtrs {
			nestedItems := chat.ExtractMessageItems(sty, nestedMsg, nestedToolResultMap, cfg)
			for _, nestedItem := range nestedItems {
				if nestedToolItem, ok := nestedItem.(chat.ToolMessageItem); ok {
					if simplifiable, ok := nestedToolItem.(chat.Compactable); ok {
						simplifiable.SetCompact(true)
					}
					c.tools = append(c.tools, nestedToolItem)

					if _, ok := nestedItem.(chat.NestedToolContainer); ok {
						deeperItems = append(deeperItems, nestedItem)
					}
				}
			}
		}
	}

	if err := loadNestedToolCalls(ctx, ws, sty, cfg, rootSessionID, generation, deeperItems); err != nil {
		return err
	}

	for i := range children {
		children[i].container.SetNestedTools(children[i].tools)
	}
	return nil
}

// appendSessionMessage appends a new message to the current session in the chat
// if the message is a tool result it will update the corresponding tool call message
func (m *UI) appendSessionMessage(msg message.Message) tea.Cmd {
	var cmds []tea.Cmd

	existing := m.chat.MessageItem(msg.ID)
	if existing != nil {
		// message already exists, skip
		return nil
	}

	switch msg.Role {
	case message.User:
		// Shell commands are rendered live via shellResultMsg; skip
		// the persisted duplicate.
		hasShellCmd := false
		for _, part := range msg.Parts {
			if _, ok := part.(message.ShellCommand); ok {
				hasShellCmd = true
				break
			}
		}
		if hasShellCmd {
			return nil
		}
		m.lastUserMessageTime = msg.CreatedAt
		items := chat.ExtractMessageItems(m.com.Styles, &msg, nil, m.com.Config())
		for _, item := range items {
			if animatable, ok := item.(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		m.chat.AppendMessages(items...)
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case message.Assistant:
		items := chat.ExtractMessageItems(m.com.Styles, &msg, nil, m.com.Config())
		for _, item := range items {
			if animatable, ok := item.(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		m.chat.AppendMessages(items...)
		if m.chat.Follow() {
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		if msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn {
			infoItem := chat.NewAssistantInfoItem(m.com.Styles, &msg, m.com.Config(), time.Unix(m.lastUserMessageTime, 0))
			m.chat.AppendMessages(infoItem)
			if m.chat.Follow() {
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	case message.Tool:
		for _, tr := range msg.ToolResults() {
			toolItem := m.chat.MessageItem(tr.ToolCallID)
			if toolItem == nil {
				// we should have an item!
				continue
			}
			if toolMsgItem, ok := toolItem.(chat.ToolMessageItem); ok {
				toolMsgItem.SetResult(&tr)
				if m.chat.Follow() {
					if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
		}
		cmds = append(cmds, m.refreshModifiedFiles())
	}
	return tea.Sequence(cmds...)
}

// updateSessionMessage updates an existing message in the current session in
// the chat when an assistant message is updated it may include updated tool
// calls as well that is why we need to handle creating/updating each tool call
// message too.
func (m *UI) updateSessionMessage(msg message.Message) tea.Cmd {
	var cmds []tea.Cmd
	existingItem := m.chat.MessageItem(msg.ID)

	if existingItem != nil {
		if assistantItem, ok := existingItem.(*chat.AssistantMessageItem); ok {
			// SetMessage returns a StartAnimation Cmd when the message
			// transitions back to spinning (e.g. its streamed content was
			// reset for a retry). Propagate it so the spinner re-arms
			// instead of freezing.
			if cmd := assistantItem.SetMessage(&msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	shouldRenderAssistant := chat.ShouldRenderAssistantMessage(&msg)
	isEndTurn := msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn
	// If the message of the assistant does not have any response just tool
	// calls we need to remove it, but keep the info item for end-of-turn
	// renders so the footer (model/provider/duration) remains visible when,
	// for example, a hook halts the turn.
	if !shouldRenderAssistant && len(msg.ToolCalls()) > 0 && existingItem != nil {
		m.chat.RemoveMessage(msg.ID)
		if !isEndTurn {
			if infoItem := m.chat.MessageItem(chat.AssistantInfoID(msg.ID)); infoItem != nil {
				m.chat.RemoveMessage(chat.AssistantInfoID(msg.ID))
			}
		}
	}

	if isEndTurn {
		if infoItem := m.chat.MessageItem(chat.AssistantInfoID(msg.ID)); infoItem == nil {
			newInfoItem := chat.NewAssistantInfoItem(m.com.Styles, &msg, m.com.Config(), time.Unix(m.lastUserMessageTime, 0))
			m.chat.AppendMessages(newInfoItem)
		}
	}

	var items []chat.MessageItem
	for _, tc := range msg.ToolCalls() {
		existingToolItem := m.chat.MessageItem(tc.ID)
		if toolItem, ok := existingToolItem.(chat.ToolMessageItem); ok {
			existingToolCall := toolItem.ToolCall()
			// only update if finished state changed or input changed
			// to avoid clearing the cache
			if (tc.Finished && !existingToolCall.Finished) || tc.Input != existingToolCall.Input {
				toolItem.SetToolCall(tc)
			}
		}
		if existingToolItem == nil {
			items = append(items, chat.NewToolMessageItem(m.com.Styles, msg.ID, tc, nil, false, m.com.Config()))
		}
	}

	for _, item := range items {
		if animatable, ok := item.(chat.Animatable); ok {
			if cmd := animatable.StartAnimation(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	m.chat.AppendMessages(items...)
	if m.chat.Follow() {
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLast()
	}

	return tea.Sequence(cmds...)
}

// childSessionRef identifies a sub-agent delegation (agent / agentic_fetch
// tool call) that can be entered as its own child session, via
// workspace.Workspace.CreateAgentToolSessionID(messageID, toolCallID).
type childSessionRef struct {
	messageID  string
	toolCallID string
}

// sessionNavFrame is one level of the sub-agent session-navigation stack
// (m.navStack): where alt+up should return to, and the sibling
// delegations in that parent chat that alt+left/alt+right cycle through.
type sessionNavFrame struct {
	parentSessionID string
	// parentTitle is the parent session's title, captured at
	// enterChildSession time. m.session is repointed to the child as soon
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

// viewingChildSession reports whether the UI is currently navigated into a
// sub-agent's session (i.e. the nav stack is non-empty). Used to make child
// sessions read-only and to keep the editor from stealing focus while one
// is being viewed.
func (m *UI) viewingChildSession() bool {
	return len(m.navStack) > 0
}

// childSessionSiblingCount returns the number of sibling delegations in the
// top nav-stack frame, i.e. how many sub-agents alt+left/alt+right can cycle
// through. Zero when not viewing a child session.
func (m *UI) childSessionSiblingCount() int {
	if len(m.navStack) == 0 {
		return 0
	}
	return len(m.navStack[len(m.navStack)-1].siblings)
}

// childSessionBreadcrumbMaxLen is the approximate max length of the prompt
// snippet shown in the child-session breadcrumb before it's truncated.
const childSessionBreadcrumbMaxLen = 40

// childSessionLabel builds a short label for a nested-tool-container item
// (agent / agentic_fetch delegation), taken from the first line of its
// running prompt. Falls back to "subagent" if the item's input can't be
// parsed or carries no prompt.
func childSessionLabel(item chat.ToolMessageItem) string {
	var p struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(item.ToolCall().Input), &p); err != nil {
		return "subagent"
	}
	prompt := strings.TrimSpace(p.Prompt)
	if prompt == "" {
		return "subagent"
	}
	if i := strings.IndexByte(prompt, '\n'); i >= 0 {
		prompt = prompt[:i]
	}
	if r := []rune(prompt); len(r) > childSessionBreadcrumbMaxLen {
		prompt = string(r[:childSessionBreadcrumbMaxLen]) + "…"
	}
	return prompt
}

// enterChildSession pushes a navigation frame for the currently loaded
// session and returns a tea.Cmd that loads the child (sub-agent) session
// identified by messageID/toolCallID. The sibling list is built from the
// nested-tool-container items already loaded in the chat, so cycling with
// alt+left/alt+right doesn't require re-fetching the parent.
//
// Restoring the parent's exact scroll position on the way back is
// deliberately not attempted — loadSession's normal load path already
// leaves a freshly loaded session in a reasonable default scroll state,
// and reconstructing the old viewport would be fragile and not worth the
// complexity for this step.
func (m *UI) enterChildSession(messageID, toolCallID string) tea.Cmd {
	childID := m.com.Workspace.CreateAgentToolSessionID(messageID, toolCallID)

	// m.session still refers to the parent here — loadSession is async and
	// doesn't repoint it synchronously — so this is the last cheap chance
	// to capture the parent's title for the breadcrumb.
	parentTitle := m.session.Title

	siblings := m.chat.NestedToolContainerRefs()
	siblingIndex := 0
	for i, s := range siblings {
		if s.messageID == messageID && s.toolCallID == toolCallID {
			siblingIndex = i
			break
		}
	}

	label := "subagent"
	var agentName, model, effort string
	var delegationStart time.Time
	var delegationDuration time.Duration
	if item, ok := m.chat.MessageItem(toolCallID).(chat.ToolMessageItem); ok {
		label = childSessionLabel(item)
		agentName, model, effort, delegationStart, delegationDuration = delegationInfo(item)
	}

	m.navStack = append(m.navStack, sessionNavFrame{
		parentSessionID:    m.session.ID,
		parentTitle:        parentTitle,
		label:              label,
		siblings:           siblings,
		siblingIndex:       siblingIndex,
		agentName:          agentName,
		model:              model,
		effort:             effort,
		delegationStart:    delegationStart,
		delegationDuration: delegationDuration,
	})

	// Child sessions are read-only: keep focus/keys on the chat list and
	// don't let the editor hold focus while viewing one.
	m.focus = uiFocusMain
	m.editor.textarea.Blur()

	// Orientation ("main › agent1 (2/3)", model/effort, tokens, state) now
	// lives entirely in drawChildSessionPanel, which replaces the editor —
	// this used to also post a status-bar breadcrumb, but InfoTypeInfo is
	// styled identically to InfoTypeSuccess (see quickstyle.go), so it
	// rendered as a full-width green bar under the panel. Redundant with
	// the panel and visually loud for what's just a location cue.

	return m.requestSessionLoad(childID)
}

// exitChildSession pops the top navigation frame and returns a tea.Cmd
// that loads the session it points back to. No-op if the stack is empty
// (e.g. alt+up pressed on a top-level session).
func (m *UI) exitChildSession() tea.Cmd {
	if len(m.navStack) == 0 {
		return nil
	}
	frame := m.navStack[len(m.navStack)-1]
	m.navStack = m.navStack[:len(m.navStack)-1]
	if len(m.navStack) == 0 {
		// Back at a top-level session: restore normal editor focus, since
		// Tab no longer offers a manual way back in.
		m.focus = uiFocusEditor
		m.chat.Blur()
		return tea.Batch(m.requestSessionLoad(frame.parentSessionID), m.editor.textarea.Focus())
	}
	return m.requestSessionLoad(frame.parentSessionID)
}

// cycleChildSession moves the sibling index of the current navigation
// frame by delta (wrapping) and returns a tea.Cmd that loads the newly
// selected sibling's child session. It updates the existing top frame in
// place rather than pushing a new one — the delegations are still
// siblings under the same parent. No-op if there's no active frame or
// fewer than two siblings to cycle through.
func (m *UI) cycleChildSession(delta int) tea.Cmd {
	if len(m.navStack) == 0 {
		return nil
	}
	frame := &m.navStack[len(m.navStack)-1]
	n := len(frame.siblings)
	if n < 2 {
		return nil
	}
	frame.siblingIndex = ((frame.siblingIndex+delta)%n + n) % n
	sibling := frame.siblings[frame.siblingIndex]

	// The sibling's own tool-call item generally isn't in m.chat here —
	// m.chat currently holds the child session we're navigating away from,
	// not the parent — so this lookup routinely misses and falls back to
	// the generic "subagent" label. frame.parentTitle was captured at
	// enterChildSession time and doesn't have that problem.
	label := "subagent"
	var agentName, model, effort string
	var delegationStart time.Time
	var delegationDuration time.Duration
	if item, ok := m.chat.MessageItem(sibling.toolCallID).(chat.ToolMessageItem); ok {
		label = childSessionLabel(item)
		agentName, model, effort, delegationStart, delegationDuration = delegationInfo(item)
	}
	frame.label = label
	frame.agentName, frame.model, frame.effort = agentName, model, effort
	frame.delegationStart, frame.delegationDuration = delegationStart, delegationDuration

	return m.requestSessionLoad(m.com.Workspace.CreateAgentToolSessionID(sibling.messageID, sibling.toolCallID))
}

// findNestedToolContainer looks up the top-level tool item in the chat
// whose tool call ID matches toolCallID and that can hold nested
// child-session tool calls (agent / agentic_fetch / custom-agent
// delegations — all three construct an AgentToolMessageItem, see
// chat.NewToolMessageItem). Returns nil if the item is missing or isn't a
// nested-tool container, e.g. a plain (non-delegating) tool call.
func (m *UI) findNestedToolContainer(toolCallID string) chat.NestedToolContainer {
	item := m.chat.MessageItem(toolCallID)
	if item == nil {
		return nil
	}
	toolMessageItem, ok := item.(chat.ToolMessageItem)
	if !ok || toolMessageItem.ToolCall().ID != toolCallID {
		return nil
	}
	container, ok := item.(chat.NestedToolContainer)
	if !ok {
		return nil
	}
	return container
}

// handleChildSessionUpdate propagates a child agent-tool session's running
// token count and todo list up to the parent delegation's block. Best-
// effort: it's a no-op when the session isn't an agent-tool child session,
// or the parent item can't be found (e.g. scrolled out of the loaded
// window).
func (m *UI) handleChildSessionUpdate(payload session.Session) {
	_, toolCallID, ok := m.com.Workspace.ParseAgentToolSessionID(payload.ID)
	if !ok {
		return
	}
	container := m.findNestedToolContainer(toolCallID)
	if container == nil {
		return
	}
	if tracker, ok := container.(chat.ChildSessionTokenTracker); ok {
		tracker.SetChildSessionTokens(payload.PromptTokens, payload.CompletionTokens)
	}
	if tracker, ok := container.(chat.ChildSessionTodoTracker); ok {
		tracker.SetChildSessionTodos(payload.Todos)
	}
}

// handleChildSessionMessage handles messages from child sessions (agent tools).
func (m *UI) handleChildSessionMessage(event pubsub.Event[message.Message]) tea.Cmd {
	var cmds []tea.Cmd

	// Only process messages with tool calls or results.
	if len(event.Payload.ToolCalls()) == 0 && len(event.Payload.ToolResults()) == 0 {
		return nil
	}

	// Check if this is an agent tool session and parse it.
	childSessionID := event.Payload.SessionID
	_, toolCallID, ok := m.com.Workspace.ParseAgentToolSessionID(childSessionID)
	if !ok {
		return nil
	}

	agentItem := m.findNestedToolContainer(toolCallID)
	if agentItem == nil {
		return nil
	}

	// Get existing nested tools.
	nestedTools := agentItem.NestedTools()

	// Update or create nested tool calls.
	for _, tc := range event.Payload.ToolCalls() {
		found := false
		for _, existingTool := range nestedTools {
			if existingTool.ToolCall().ID == tc.ID {
				existingTool.SetToolCall(tc)
				found = true
				break
			}
		}
		if !found {
			// Create a new nested tool item.
			nestedItem := chat.NewToolMessageItem(m.com.Styles, event.Payload.ID, tc, nil, false, m.com.Config())
			if simplifiable, ok := nestedItem.(chat.Compactable); ok {
				simplifiable.SetCompact(true)
			}
			if animatable, ok := nestedItem.(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			nestedTools = append(nestedTools, nestedItem)
		}
	}

	// Update nested tool results.
	for _, tr := range event.Payload.ToolResults() {
		for _, nestedTool := range nestedTools {
			if nestedTool.ToolCall().ID == tr.ToolCallID {
				nestedTool.SetResult(&tr)
				break
			}
		}
	}

	// Update the agent item with the new nested tools.
	agentItem.SetNestedTools(nestedTools)

	// Update the chat so it updates the index map for animations to work as expected
	m.chat.UpdateNestedToolIDs(toolCallID)

	if m.chat.Follow() {
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLast()
	}

	return tea.Sequence(cmds...)
}

func (m *UI) handleDialogMsg(msg tea.Msg) tea.Cmd {
	action := m.dialog.Update(msg)
	if action == nil {
		return tea.Batch()
	}
	return m.applyDialogAction(action)
}

// applyDialogAction executes a [dialog.Action] regardless of where it came
// from: a dialog's HandleMsg (the usual path, via handleDialogMsg) or a
// command selected directly from the editor's "/" completion popup, which
// has no dialog on the stack at all. CloseDialog/CloseFrontDialog are no-ops
// when their target isn't open, so callers never need to special-case the
// no-dialog path.
func (m *UI) applyDialogAction(action dialog.Action) tea.Cmd {
	var cmds []tea.Cmd
	isOnboarding := m.state == uiOnboarding

	switch msg := action.(type) {
	// Generic dialog messages
	case dialog.ActionClose:
		if isOnboarding && m.dialog.ContainsDialog(dialog.ProvidersID) {
			break
		}

		if m.dialog.ContainsDialog(dialog.FilePickerID) {
			defer fimage.ResetCache()
		}

		m.dialog.CloseFrontDialog()

		if isOnboarding {
			if cmd := m.openProvidersDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

		if m.focus == uiFocusEditor {
			cmds = append(cmds, m.editor.textarea.Focus())
		}
	case dialog.ActionCmd:
		if msg.Cmd != nil {
			cmds = append(cmds, msg.Cmd)
		}

	// Session dialog messages.
	case dialog.ActionSelectSession:
		m.dialog.CloseDialog(dialog.SessionsID)
		cmds = append(cmds, m.requestSessionLoad(msg.Session.ID))

	// Open dialog message.
	case dialog.ActionOpenDialog:
		m.dialog.CloseDialog(dialog.CommandsID)
		if cmd := m.openDialog(msg.DialogID); cmd != nil {
			cmds = append(cmds, cmd)
		}

	// Command dialog messages.
	case dialog.ActionToggleYoloMode:
		cmds = append(cmds, m.toggleYoloMode())
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionSelectNotificationStyle:
		if m.notificationLoading {
			cmds = append(cmds, util.ReportWarn("Notification settings are already being updated"))
			break
		}
		style := msg.Style
		if cfg := m.com.Config(); cfg != nil && cfg.Options != nil {
			m.notificationLoading = true
			m.notificationGeneration++
			generation := m.notificationGeneration
			workspace := m.com.Workspace
			cmds = append(cmds, func() tea.Msg {
				return notificationStyleSetMsg{Err: workspace.SetConfigField(config.ScopeGlobal, "options.notifications", style), Style: style, generation: generation}
			})
		}
	case dialog.ActionNewSession:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session..."))
			break
		}
		if cmd := m.newSession(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionSummarize:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before summarizing session..."))
			break
		}
		cmds = append(cmds, func() tea.Msg {
			err := m.com.Workspace.AgentSummarize(context.Background(), msg.SessionID)
			if err != nil {
				return util.ReportError(err)()
			}
			return nil
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleHelp:
		m.status.ToggleHelp()
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionExternalEditor:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is working, please wait..."))
			break
		}
		editorValue := m.editor.textarea.Value()
		if m.editor.bangMode {
			editorValue = "!" + editorValue
		}
		cmds = append(cmds, m.openEditor(editorValue))
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleCompactMode:
		cmds = append(cmds, m.toggleCompactMode())
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionTogglePills:
		if cmd := m.toggleTodosExpanded(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleThinking:
		if m.modelOperationLoading {
			cmds = append(cmds, util.ReportWarn("Model settings are already being updated"))
			break
		}
		cfg := m.com.Config()
		if cfg == nil {
			cmds = append(cmds, util.ReportError(errors.New("configuration not found")))
			break
		}
		if _, ok := cfg.Agents[config.AgentCoder]; !ok {
			cmds = append(cmds, util.ReportError(errors.New("agent configuration not found")))
			break
		}
		currentModel := cfg.Model
		currentModel.Think = !currentModel.Think
		status := "disabled"
		if currentModel.Think {
			status = "enabled"
		}
		m.modelOperationLoading = true
		m.modelOperationGeneration++
		generation := m.modelOperationGeneration
		workspace := m.com.Workspace
		ctx := m.com.Context()
		cmds = append(cmds, func() tea.Msg {
			if err := workspace.UpdatePreferredModel(config.ScopeGlobal, currentModel); err != nil {
				return modelSettingUpdatedMsg{Err: err, generation: generation}
			}
			return modelSettingUpdatedMsg{Err: workspace.UpdateAgentModel(ctx), Info: "Thinking mode " + status, generation: generation}
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleTransparentBackground:
		if m.transparentLoading {
			cmds = append(cmds, util.ReportWarn("Transparency is already being updated"))
			break
		}
		cfg := m.com.Config()
		if cfg == nil {
			cmds = append(cmds, util.ReportError(errors.New("configuration not found")))
			break
		}
		desired := cfg.Options == nil || cfg.Options.TUI.Transparent == nil || !*cfg.Options.TUI.Transparent
		m.transparentLoading = true
		m.transparentGeneration++
		generation := m.transparentGeneration
		workspace := m.com.Workspace
		cmds = append(cmds, func() tea.Msg {
			return transparentToggledMsg{Err: workspace.SetConfigField(config.ScopeGlobal, "options.tui.transparent", desired), Enabled: desired, generation: generation}
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionQuit:
		cmds = append(cmds, tea.Quit)
	case dialog.ActionEnableDockerMCP:
		m.dialog.CloseDialog(dialog.CommandsID)
		cmds = append(cmds, m.enableDockerMCP)
	case dialog.ActionDisableDockerMCP:
		m.dialog.CloseDialog(dialog.CommandsID)
		cmds = append(cmds, m.disableDockerMCP)
	case dialog.ActionInitializeProject:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before summarizing session..."))
			break
		}
		cmds = append(cmds, m.initializeProject())
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionOpenThreadsDashboard:
		m.dialog.CloseDialog(dialog.CommandsID)
		if !m.com.Workspace.SupportsThreads() {
			cmds = append(cmds, util.ReportInfo("This workspace doesn't support threads."))
			break
		}
		cmds = append(cmds, util.CmdHandler(showThreadsDashboardMsg{}))

	case dialog.ActionSelectModel:
		if cmd := m.handleSelectModel(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ActionSelectReasoningEffort:
		if m.modelOperationLoading {
			cmds = append(cmds, util.ReportWarn("Model settings are already being updated"))
			break
		}
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait..."))
			break
		}

		cfg := m.com.Config()
		if cfg == nil {
			cmds = append(cmds, util.ReportError(errors.New("configuration not found")))
			break
		}

		if _, ok := cfg.Agents[config.AgentCoder]; !ok {
			cmds = append(cmds, util.ReportError(errors.New("agent configuration not found")))
			break
		}

		// The coder agent leaves Model unset (it inherits the app's
		// configured model), so the model it actually runs on is always
		// cfg.Model.
		currentModel := cfg.Model
		currentModel.ReasoningEffort = msg.Effort
		effort := msg.Effort

		m.modelOperationLoading = true
		m.modelOperationGeneration++
		generation := m.modelOperationGeneration
		workspace := m.com.Workspace
		ctx := m.com.Context()
		cmds = append(cmds, func() tea.Msg {
			if err := workspace.UpdatePreferredModel(config.ScopeGlobal, currentModel); err != nil {
				return modelSettingUpdatedMsg{Err: err, generation: generation}
			}
			return modelSettingUpdatedMsg{Err: workspace.UpdateAgentModel(ctx), Info: "Reasoning effort set to " + effort, generation: generation}
		})
		m.dialog.CloseDialog(dialog.ReasoningID)
	case dialog.ActionPermissionResponse:
		if m.permissionLoading {
			cmds = append(cmds, util.ReportWarn("Permission response is already being submitted"))
			break
		}
		m.permissionLoading = true
		m.permissionGeneration++
		generation := m.permissionGeneration
		action := msg.Action
		perm := msg.Permission
		m.permissionID = perm.ID
		permissionID := perm.ID
		workspace := m.com.Workspace
		cmds = append(cmds, func() tea.Msg {
			accepted := false
			switch action {
			case dialog.PermissionAllow:
				accepted = workspace.PermissionGrant(perm)
			case dialog.PermissionAllowForSession:
				accepted = workspace.PermissionGrantPersistent(perm)
			case dialog.PermissionDeny:
				accepted = workspace.PermissionDeny(perm)
			}
			return permissionResponseMsg{Accepted: accepted, Permission: permissionID, generation: generation}
		})

	case dialog.ActionFilePickerSelected:
		cmds = append(cmds, tea.Sequence(
			msg.Cmd(),
			func() tea.Msg {
				m.dialog.CloseDialog(dialog.FilePickerID)
				return nil
			},
			func() tea.Msg {
				fimage.ResetCache()
				return nil
			},
		))

	case dialog.ActionRunCustomCommand:
		if len(msg.Arguments) > 0 && msg.Args == nil {
			m.dialog.CloseFrontDialog()
			argsDialog := dialog.NewArguments(
				m.com,
				"Custom Command Arguments",
				"",
				msg.Arguments,
				msg, // Pass the action as the result
			)
			m.dialog.OpenDialog(argsDialog)
			break
		}
		content := msg.Content
		if msg.Args != nil {
			content = substituteArgs(content, msg.Args)
		}
		// If this is a skill command, format it using the skill's FormatInvocation method
		if msg.Skill != nil {
			content = msg.Skill.FormatInvocation()
		}
		cmds = append(cmds, m.sendMessage(content))
		m.dialog.CloseFrontDialog()
	case dialog.ActionAttachSkill:
		m.dialog.CloseFrontDialog()
		cmds = append(cmds, m.attachSkill(msg.ID, msg.Name))
	// Providers configuration dialog messages.
	case dialog.ActionConfigureProvider:
		if cmd := m.configureProvider(msg.ProviderID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ActionOpenCustomProviderForm:
		m.openProviderFormDialog()
	case dialog.ActionSubmitCustomProvider:
		ws := m.com.Workspace
		ctx := m.com.Context()
		params := workspace.ConfigureCustomProviderParams{
			ID:      msg.ID,
			BaseURL: msg.BaseURL,
			Type:    msg.Type,
			APIKey:  msg.APIKey,
		}
		cmds = append(cmds, func() tea.Msg {
			_, err := workspace.ConfigureCustomProvider(ctx, ws, config.ScopeGlobal, params)
			return dialog.ActionCustomProviderResult{ProviderID: msg.ID, Err: err}
		})
	case dialog.ActionProviderConfigured:
		if m.modelOperationLoading {
			cmds = append(cmds, util.ReportWarn("Model settings are already being updated"))
			break
		}
		m.dialog.CloseDialog(dialog.ProviderFormID)
		m.dialog.CloseDialog(dialog.APIKeyInputID)
		m.dialog.CloseDialog(dialog.OAuthID)
		m.dialog.CloseDialog(dialog.ProvidersID)

		if m.state != uiOnboarding {
			break
		}

		cfg := m.com.Config()
		ws := m.com.Workspace

		model := cfg.Model
		if cfg.GetModel(model.Provider, model.Model) == nil {
			// No valid model carried over (this is the normal first-run
			// case, where no model has ever been selected) — fall back to
			// this provider's own default rather than whatever
			// defaultModelSelection would pick globally.
			knownProviders, err := config.Providers(cfg)
			if err != nil && len(knownProviders) == 0 {
				cmds = append(cmds, util.ReportError(err))
				break
			}
			def, err := cfg.DefaultModelForProvider(msg.ProviderID, knownProviders)
			if err != nil {
				cmds = append(cmds, util.ReportError(err))
				break
			}
			model = def
		}

		// Move UpdatePreferredModel into a tea.Cmd so it does not block
		// Update.  The result (providerConfiguredResult) is handled in
		// Update and only calls initAgentAndReportModel on success.
		m.modelOperationLoading = true
		m.modelOperationGeneration++
		generation := m.modelOperationGeneration
		capturedModel := model
		cmds = append(cmds, func() tea.Msg {
			if err := ws.UpdatePreferredModel(config.ScopeGlobal, capturedModel); err != nil {
				return providerConfiguredResult{Err: err, generation: generation}
			}
			return providerConfiguredResult{Model: capturedModel, Onboarding: true, generation: generation}
		})

	case dialog.ActionRunMCPPrompt:
		if len(msg.Arguments) > 0 && msg.Args == nil {
			m.dialog.CloseFrontDialog()
			title := cmp.Or(msg.Title, "MCP Prompt Arguments")
			argsDialog := dialog.NewArguments(
				m.com,
				title,
				msg.Description,
				msg.Arguments,
				msg, // Pass the action as the result
			)
			m.dialog.OpenDialog(argsDialog)
			break
		}
		cmds = append(cmds, m.runMCPPrompt(msg.ClientID, msg.PromptID, msg.Args))
	default:
		cmds = append(cmds, util.CmdHandler(msg))
	}

	return tea.Batch(cmds...)
}

// substituteArgs replaces $ARG_NAME placeholders in content with actual values.
func substituteArgs(content string, args map[string]string) string {
	for name, value := range args {
		placeholder := "$" + name
		content = strings.ReplaceAll(content, placeholder, value)
	}
	return content
}

// handleSelectModel performs the model selection after any provider
// pre-checks have completed.  The ImportCopilot, UpdatePreferredModel, and
// initAgentAndReportModel steps run sequentially via typed result messages;
// errors stop the chain without a false success.
func (m *UI) handleSelectModel(msg dialog.ActionSelectModel) tea.Cmd {
	var cmds []tea.Cmd

	if m.modelOperationLoading {
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
		m.modelOperationLoading = true
		m.modelOperationGeneration++
		generation := m.modelOperationGeneration
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
		if cmd := m.openAuthenticationDialog(msg.Provider, msg.Model, msg.ModelType); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...)
	}

	// Move UpdatePreferredModel into the cmd; the result is handled by a
	// modelSelectResult case that only calls initAgentAndReportModel on success.
	m.modelOperationLoading = true
	m.modelOperationGeneration++
	generation := m.modelOperationGeneration
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

func (m *UI) openAuthenticationDialog(provider catwalk.Provider, model config.SelectedModel, modelType config.SelectedModelType) tea.Cmd {
	var (
		dlg dialog.Dialog
		cmd tea.Cmd

		isOnboarding = m.state == uiOnboarding
	)

	switch provider.ID {
	case catwalk.InferenceProviderCopilot:
		dlg, cmd = dialog.NewOAuthCopilot(m.com, isOnboarding, provider, &model, modelType)
	default:
		dlg, cmd = dialog.NewAPIKeyInput(m.com, isOnboarding, provider, &model, modelType)
	}

	if m.dialog.ContainsDialog(dlg.ID()) {
		m.dialog.BringToFront(dlg.ID())
		return nil
	}

	m.dialog.OpenDialogWithGrace(dlg)
	return cmd
}

func (m *UI) handleKeyPressMsg(msg tea.KeyPressMsg) tea.Cmd {
	var cmds []tea.Cmd

	handleGlobalKeys := func(msg tea.KeyPressMsg) bool {
		switch {
		case key.Matches(msg, m.keyMap.Help):
			m.status.ToggleHelp()
			m.updateLayoutAndSize()
			return true
		case key.Matches(msg, m.keyMap.Commands):
			if cmd := m.openCommandsDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return true
		case key.Matches(msg, m.keyMap.Models):
			if cmd := m.openModelsDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return true
		case key.Matches(msg, m.keyMap.Sessions):
			if cmd := m.openSessionsDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return true
		case key.Matches(msg, m.keyMap.Threads):
			if !m.com.Workspace.SupportsThreads() {
				cmds = append(cmds, util.ReportInfo("This workspace doesn't support threads."))
				return true
			}
			cmds = append(cmds, util.CmdHandler(showThreadsDashboardMsg{}))
			return true
		case key.Matches(msg, m.keyMap.Chat.Details) && m.isCompact:
			m.detailsOpen = !m.detailsOpen
			m.updateLayoutAndSize()
			return true
		case key.Matches(msg, m.keyMap.Chat.TogglePills):
			if m.state == uiChat && m.hasSession() {
				if cmd := m.toggleTodosExpanded(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return true
			}
		case key.Matches(msg, m.keyMap.Suspend):
			if m.isAgentBusy() {
				cmds = append(cmds, util.ReportWarn("Agent is busy, please wait..."))
				return true
			}
			cmds = append(cmds, tea.Suspend)
			return true
		case key.Matches(msg, m.keyMap.ToggleYolo):
			cmds = append(cmds, m.toggleYoloMode())
			return true
		}
		return false
	}

	if key.Matches(msg, m.keyMap.Quit) && !m.dialog.ContainsDialog(dialog.QuitID) {
		// Always handle quit keys first
		if cmd := m.openQuitDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}

		return tea.Batch(cmds...)
	}

	// Route all messages to dialog if one is open.
	if m.dialog.HasDialogs() {
		return m.handleDialogMsg(msg)
	}

	// Route keys to active inline editor if one is showing.
	if m.activeInline != nil && m.focus == uiFocusEditor {
		if done, cmd := m.activeInline.HandleKey(msg); done {
			m.activeInline = nil
			m.editor.textarea.Focus()
			m.updateLayoutAndSize()
		} else {
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.activeInline.HeightChanged() {
				m.updateLayoutAndSize()
			}
		}
		return tea.Batch(cmds...)
	}

	// Handle cancel key when agent is busy.
	if key.Matches(msg, m.keyMap.Chat.Cancel) {
		if m.isAgentBusy() {
			if cmd := m.cancelAgent(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return tea.Batch(cmds...)
		}
	}

	switch m.state {
	case uiOnboarding:
		return tea.Batch(cmds...)
	case uiInitialize:
		cmds = append(cmds, m.updateInitializeView(msg)...)
		return tea.Batch(cmds...)
	case uiChat, uiLanding:
		switch m.focus {
		case uiFocusEditor:
			// Double-Esc clears the draft outright (see the Editor.Escape
			// case below): any key other than Escape breaks the sequence.
			if !key.Matches(msg, m.keyMap.Editor.Escape) {
				m.editor.lastKeyWasEsc = false
			}

			// Handle completions if open.
			if m.editor.completionsOpen {
				if msg, ok := m.editor.completions.Update(msg); ok {
					switch msg := msg.(type) {
					case completions.SelectionMsg[completions.FileCompletionValue]:
						cmds = append(cmds, m.insertFileCompletion(msg.Value.Path))
						if !msg.KeepOpen {
							m.editor.closeCompletions()
						}
					case completions.SelectionMsg[completions.ResourceCompletionValue]:
						cmds = append(cmds, m.insertMCPResourceCompletion(msg.Value))
						if !msg.KeepOpen {
							m.editor.closeCompletions()
						}
					case completions.SelectionMsg[completions.CommandCompletionValue]:
						if msg.InsertOnly {
							// Tab: fill in the command name so the user can
							// type arguments, without running it.
							m.editor.insertCompletionText("/" + msg.Value.Title)
						} else {
							// Enter: run the command immediately and clear
							// the editor, same as picking it from the
							// Commands palette.
							m.editor.textarea.Reset()
							if action, ok := msg.Value.Action.(dialog.Action); ok {
								cmds = append(cmds, m.applyDialogAction(action))
							}
						}
						m.editor.closeCompletions()
					case completions.ClosedMsg:
						m.editor.closeCompletions()
					}
					return tea.Batch(cmds...)
				}
			}

			if ok := m.editor.attachments.Update(msg); ok {
				return tea.Batch(cmds...)
			}

			switch {
			case key.Matches(msg, m.keyMap.Editor.AddImage):
				if !m.currentModelSupportsImages() {
					break
				}
				if cmd := m.openFilesDialog(); cmd != nil {
					cmds = append(cmds, cmd)
				}

			case key.Matches(msg, m.keyMap.Editor.PasteImage):
				if !m.currentModelSupportsImages() {
					break
				}
				cmds = append(cmds, m.pasteImageFromClipboard)

			case key.Matches(msg, m.keyMap.Editor.SendMessage):
				prevHeight := m.editor.textarea.Height()
				value := m.editor.textarea.Value()
				if before, ok := strings.CutSuffix(value, "\\"); ok {
					// If the last character is a backslash, remove it and add a newline.
					m.editor.textarea.SetValue(before)
					if cmd := m.handleTextareaHeightChange(prevHeight); cmd != nil {
						cmds = append(cmds, cmd)
					}
					break
				}

				// Otherwise, send the message
				m.editor.textarea.Reset()
				if cmd := m.handleTextareaHeightChange(prevHeight); cmd != nil {
					cmds = append(cmds, cmd)
				}

				value = strings.TrimSpace(value)
				if value == "exit" || value == "quit" {
					return m.openQuitDialog()
				}

				if m.editor.bangMode && value != "" {
					m.editor.bangMode = false
					m.setEditorPrompt(m.yoloModeCached())
					m.randomizePlaceholders()
					m.editor.historyReset()
					return tea.Batch(m.runShellCommand(value))
				}

				attachments := m.editor.attachments.List()
				m.editor.attachments.Reset()
				if len(value) == 0 && len(attachments) == 0 {
					return nil
				}

				m.randomizePlaceholders()
				m.editor.historyReset()

				return tea.Batch(m.sendMessage(value, attachments...), m.loadPromptHistory())
			case key.Matches(msg, m.keyMap.Chat.NewSession):
				if !m.hasSession() {
					break
				}
				if m.isAgentBusy() {
					cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session..."))
					break
				}
				if cmd := m.newSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Editor.OpenEditor):
				if m.isAgentBusy() {
					cmds = append(cmds, util.ReportWarn("Agent is working, please wait..."))
					break
				}
				editorValue := m.editor.textarea.Value()
				if m.editor.bangMode {
					editorValue = "!" + editorValue
				}
				cmds = append(cmds, m.openEditor(editorValue))
			case key.Matches(msg, m.keyMap.Editor.Newline):
				prevHeight := m.editor.textarea.Height()
				m.editor.textarea.InsertRune('\n')
				m.editor.closeCompletions()
				cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))
			case key.Matches(msg, m.keyMap.Editor.HistoryPrev):
				cmd := m.handleHistoryUp(msg)
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Editor.HistoryNext):
				cmd := m.handleHistoryDown(msg)
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Tab):
				// Tab accepts the inline history prediction, if one is
				// showing. It's otherwise a no-op here (deliberately: it
				// no longer toggles focus, and must not fall through to
				// the default branch below, which would hand it to the
				// raw textarea and insert a literal tab character).
				if tail := m.activeGhostTail(); tail != "" {
					prevHeight := m.editor.textarea.Height()
					m.editor.textarea.InsertString(tail)
					m.editor.textarea.MoveToEnd()
					if cmd := m.updateTextareaWithPrevHeight(nil, prevHeight); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			case key.Matches(msg, m.keyMap.Editor.Escape):
				consecutive := m.editor.lastKeyWasEsc
				m.editor.lastKeyWasEsc = true
				if !consecutive {
					// First Esc: its own job (exit history nav to the draft,
					// or whatever the textarea itself does with Escape).
					if cmd := m.handleHistoryEscape(msg); cmd != nil {
						cmds = append(cmds, cmd)
					}
					// Hide any active ghost prediction until the input
					// changes again. Applied after handleHistoryEscape so
					// it hides relative to the value Esc left behind (e.g.
					// the restored draft), not the pre-Esc value.
					m.editor.ghostHiddenFor = m.editor.textarea.Value()
				} else {
					// Second Esc right after the first: the first one
					// already did whatever it could (exited history nav,
					// etc.), so this one wipes the draft outright instead
					// of leaving stale text sitting in the editor.
					prevHeight := m.editor.textarea.Height()
					m.editor.promptHistory.index = -1
					m.editor.promptHistory.draft = ""
					m.editor.textarea.Reset()
					m.syncBangModeFromTextarea()
					if cmd := m.updateTextareaWithPrevHeight(nil, prevHeight); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			default:
				if handleGlobalKeys(msg) {
					// Handle global keys first before passing to textarea.
					break
				}

				// Bang mode: backspace on already-empty prompt exits.
				if m.editor.bangMode && m.editor.bangWasEmpty && msg.Code == tea.KeyBackspace {
					m.editor.bangMode = false
					m.editor.bangWasEmpty = false
					m.setEditorPrompt(m.yoloModeCached())
					break
				}

				// Check for @ trigger before passing to textarea.
				curValue := m.editor.textarea.Value()
				curIdx := len(curValue)

				// Trigger completions on @. Suppressed in bang mode: "@" is
				// just a character in a shell command (e.g. "git log @{u}"),
				// not a file-mention trigger.
				if msg.String() == "@" && !m.editor.completionsOpen && !m.editor.bangMode {
					// Only show if beginning of prompt or after whitespace.
					if curIdx == 0 || (curIdx > 0 && isWhitespace(curValue[curIdx-1])) {
						m.editor.completionsOpen = true
						m.editor.completionsMode = completionsModeFile
						m.editor.completionsQuery = ""
						m.editor.completionsStartIndex = curIdx
						m.editor.completionsPositionStart = m.completionsPosition()
						depth, limit := m.com.Config().Options.TUI.Completions.Limits()
						m.editor.completions.SetMaxWidth(m.completionsMaxWidth())
						cmds = append(cmds, m.editor.completions.Open(depth, limit, m.loadMCPResourceCompletions))
					}
				}

				// Trigger command completions on "/" at the very start of an
				// otherwise-empty editor, mirroring opencode/Claude Code: a
				// "/" mid-message is just a character. Suppressed in bang
				// mode: a shell command is very plausibly an absolute path
				// like "/usr/bin/env", not a command trigger.
				if msg.String() == "/" && !m.editor.completionsOpen && !m.editor.bangMode && curValue == "" {
					m.editor.completionsOpen = true
					m.editor.completionsMode = completionsModeCommand
					m.editor.completionsQuery = ""
					m.editor.completionsStartIndex = curIdx
					m.editor.completionsPositionStart = m.completionsPosition()
					m.editor.completions.SetMaxWidth(m.completionsMaxWidth())
					m.editor.completions.OpenCommands(m.commandCompletionItems())
				}

				// remove the details if they are open when user starts typing
				if m.detailsOpen {
					m.detailsOpen = false
					m.updateLayoutAndSize()
				}

				prevHeight := m.editor.textarea.Height()
				cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))

				// Bang mode: enter when "!" is typed at the start of the
				// prompt, optionally preceded by whitespace (either on an
				// empty/whitespace-only prompt or prepended to existing text).
				// Exit on backspace clearing the last character.
				newVal := m.editor.textarea.Value()
				trimmedNew := strings.TrimLeftFunc(newVal, unicode.IsSpace)
				trimmedCur := strings.TrimLeftFunc(curValue, unicode.IsSpace)
				if !m.editor.bangMode && strings.HasPrefix(trimmedNew, "!") && !strings.HasPrefix(trimmedCur, "!") {
					m.editor.bangMode = true
					m.editor.bangWasEmpty = len(strings.TrimSpace(curValue)) == 0
					// Strip leading whitespace and the "!" from the textarea
					// while preserving the cursor position relative to the
					// command text.
					col := m.editor.textarea.Column()
					line := m.editor.textarea.Line()
					stripped := trimmedNew[1:]
					m.editor.textarea.SetValue(stripped)
					m.editor.textarea.SetCursorColumn(max(0, col-(len(newVal)-len(stripped))))
					_ = line // cursor line doesn't change; prefix removed
					m.setEditorPrompt(m.yoloModeCached())
				} else if m.editor.bangMode && newVal == "" && curValue != "" {
					// Just cleared last character; mark empty, stay in bang mode.
					m.editor.bangWasEmpty = true
				} else if m.editor.bangMode && newVal != "" {
					m.editor.bangWasEmpty = false
				}

				// Any text modification becomes the current draft.
				m.editor.updateHistoryDraft(curValue)

				// After updating textarea, check if we need to filter completions.
				// Skip filtering on the initial @ or / keystroke: for @ the
				// items are still loading async, and for / the query is
				// empty anyway (OpenCommands already populated the list).
				if m.editor.completionsOpen && msg.String() != "@" && msg.String() != "/" {
					newValue := m.editor.textarea.Value()
					newIdx := len(newValue)

					// Close completions if cursor moved before start.
					if newIdx <= m.editor.completionsStartIndex {
						m.editor.closeCompletions()
					} else if msg.String() == "space" {
						// Close on space.
						m.editor.closeCompletions()
					} else {
						// Extract current word and filter.
						triggerChar := "@"
						if m.editor.completionsMode == completionsModeCommand {
							triggerChar = "/"
						}
						word := m.editor.textareaWord()
						if strings.HasPrefix(word, triggerChar) {
							m.editor.completionsQuery = word[1:]
							m.editor.completions.Filter(m.editor.completionsQuery)
						} else if m.editor.completionsOpen {
							m.editor.closeCompletions()
						}
					}
				}
			}
		case uiFocusMain:
			switch {
			case key.Matches(msg, m.keyMap.Chat.NewSession):
				if !m.hasSession() {
					break
				}
				if m.isAgentBusy() {
					cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session..."))
					break
				}
				m.focus = uiFocusEditor
				if cmd := m.newSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Chat.Expand):
				m.chat.ToggleExpandedSelectedItem()
			case key.Matches(msg, m.keyMap.Chat.EnterChildSession) && m.state == uiChat && m.hasSession():
				if messageID, toolCallID, ok := m.chat.SelectedNestedToolContainer(); ok {
					if cmd := m.enterChildSession(messageID, toolCallID); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			case key.Matches(msg, m.keyMap.Chat.ExitChildSession) && m.state == uiChat && m.hasSession():
				if cmd := m.exitChildSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Chat.PrevChildSession) && m.state == uiChat && m.hasSession():
				if cmd := m.cycleChildSession(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Chat.NextChildSession) && m.state == uiChat && m.hasSession():
				if cmd := m.cycleChildSession(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Chat.Up):
				if cmd := m.chat.ScrollByAndAnimate(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectPrev()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			case key.Matches(msg, m.keyMap.Chat.Down):
				if cmd := m.chat.ScrollByAndAnimate(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectNext()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			case key.Matches(msg, m.keyMap.Chat.UpOneItem):
				m.chat.SelectPrev()
				if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Chat.DownOneItem):
				m.chat.SelectNext()
				if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Chat.HalfPageUp):
				if cmd := m.chat.ScrollByAndAnimate(-m.chat.Height() / 2); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectFirstInView()
			case key.Matches(msg, m.keyMap.Chat.HalfPageDown):
				if cmd := m.chat.ScrollByAndAnimate(m.chat.Height() / 2); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectLastInView()
			case key.Matches(msg, m.keyMap.Chat.PageUp):
				if cmd := m.chat.ScrollByAndAnimate(-m.chat.Height()); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectFirstInView()
			case key.Matches(msg, m.keyMap.Chat.PageDown):
				if cmd := m.chat.ScrollByAndAnimate(m.chat.Height()); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectLastInView()
			case key.Matches(msg, m.keyMap.Chat.Home):
				if cmd := m.chat.ScrollToTopAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectFirst()
			case key.Matches(msg, m.keyMap.Chat.End):
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectLast()
			default:
				if ok, cmd := m.chat.HandleKeyMsg(msg); ok {
					cmds = append(cmds, cmd)
				} else {
					handleGlobalKeys(msg)
				}
			}
		default:
			handleGlobalKeys(msg)
		}
	default:
		handleGlobalKeys(msg)
	}

	return tea.Sequence(cmds...)
}

// drawHeader draws the header section of the UI.
func (m *UI) drawHeader(scr uv.Screen, area uv.Rectangle) {
	m.header.drawHeader(
		scr,
		area,
		m.session,
		m.isCompact,
		m.detailsOpen,
		area.Dx(),
		m.lspErrorCount(),
		m.threadIndicator.count,
		bindingKey(m.keyMap.Chat.Details),
	)
}

// Draw implements [uv.Drawable] and draws the UI model.
func (m *UI) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	layout := m.generateLayout(area.Dx(), area.Dy())

	if m.layout != layout {
		m.layout = layout
		m.updateSize()
	}

	if m.state == uiChat && m.hasSession() && !m.isCompact {
		m.updateSidebarScrollState()
	}

	// Clear the screen first
	screen.Clear(scr)

	switch m.state {
	case uiOnboarding:
		m.drawHeader(scr, layout.header)

		// NOTE: Onboarding flow will be rendered as dialogs below, but
		// positioned at the bottom left of the screen.

	case uiInitialize:
		m.drawHeader(scr, layout.header)

		main := uv.NewStyledString(m.initializeView())
		main.Draw(scr, layout.main)

	case uiLanding:
		m.drawHeader(scr, layout.header)
		main := uv.NewStyledString(m.landingView())
		main.Draw(scr, layout.main)

		if m.activeInline != nil {
			m.activeInline.SetFocused(m.focus == uiFocusEditor)
			if m.focus == uiFocusEditor {
				m.inlineCursor = m.activeInline.Draw(scr, layout.editor)
			} else if qf, ok := m.activeInline.(*dialog.QuestionForm); ok && m.shouldCollapseQuestion(qf) {
				qf.DrawCollapsed(scr, layout.editor)
				m.inlineCursor = nil
			} else {
				m.inlineCursor = m.activeInline.Draw(scr, layout.editor)
			}
		} else {
			editor := uv.NewStyledString(m.renderEditorView(scr.Bounds().Dx()))
			editor.Draw(scr, layout.editor)
			m.drawGhostText(scr)
			m.inlineCursor = nil
		}

	case uiChat:
		if m.isCompact {
			m.drawHeader(scr, layout.header)
		} else {
			m.drawSidebar(scr, layout.sidebar)
		}

		m.chat.Draw(scr, layout.main)
		if layout.panel.Dy() > 0 {
			m.drawSessionPanel(scr, layout.panel)
		}

		if m.viewingChildSession() {
			// The child-session info panel replaces the editor entirely
			// while a sub-agent session is being viewed — no textarea, no
			// ghost text, no cursor (see uiFocusState: focus is forced to
			// uiFocusMain for the duration).
			m.drawChildSessionPanel(scr, layout.editor)
			m.inlineCursor = nil
		} else if m.activeInline != nil {
			m.activeInline.SetFocused(m.focus == uiFocusEditor)
			if m.focus == uiFocusEditor {
				m.inlineCursor = m.activeInline.Draw(scr, layout.editor)
			} else if qf, ok := m.activeInline.(*dialog.QuestionForm); ok && m.shouldCollapseQuestion(qf) {
				qf.DrawCollapsed(scr, layout.editor)
				m.inlineCursor = nil
			} else {
				m.inlineCursor = m.activeInline.Draw(scr, layout.editor)
			}
		} else {
			editorWidth := scr.Bounds().Dx()
			if !m.isCompact {
				editorWidth -= layout.sidebar.Dx()
			}
			editor := uv.NewStyledString(m.renderEditorView(editorWidth))
			editor.Draw(scr, layout.editor)
			m.drawGhostText(scr)
			m.inlineCursor = nil
		}
		// Draw the input separators after the editor so its content cannot
		// cover the reserved boundary rows.
		m.drawChatSeparators(scr, layout.editor)

		// Draw details overlay in compact mode when open
		if m.isCompact && m.detailsOpen {
			m.drawSessionDetails(scr, layout.sessionDetails)
		}
	}

	isOnboarding := m.state == uiOnboarding

	// Add status and help layer
	m.status.SetHideHelp(isOnboarding)
	m.status.Draw(scr, layout.status)

	// Draw completions popup if open
	if !isOnboarding && m.editor.completionsOpen && m.editor.completions.HasItems() {
		w, h := m.editor.completions.Size()
		x := m.editor.completionsPositionStart.X
		y := m.editor.completionsPositionStart.Y - h

		screenW := area.Dx()
		if x+w > screenW {
			x = screenW - w
		}
		x = max(0, x)
		y = max(0, y+m.editorAttachmentsRowOffset())

		completionsView := uv.NewStyledString(m.editor.completions.Render())
		completionsView.Draw(scr, image.Rectangle{
			Min: image.Pt(x, y),
			Max: image.Pt(x+w, y+h),
		})
	}

	// Debugging rendering (visually see when the tui rerenders)
	if os.Getenv("BRAID_UI_DEBUG") == "true" {
		debugView := lipgloss.NewStyle().Background(lipgloss.ANSIColor(rand.Intn(256))).Width(4).Height(2)
		debug := uv.NewStyledString(debugView.String())
		debug.Draw(scr, image.Rectangle{
			Min: image.Pt(4, 1),
			Max: image.Pt(8, 3),
		})
	}

	// This needs to come last to overlay on top of everything. We always pass
	// the full screen bounds because the dialogs will position themselves
	// accordingly.
	if m.dialog.HasDialogs() {
		return m.dialog.Draw(scr, scr.Bounds())
	}

	switch m.focus {
	case uiFocusEditor:
		if m.layout.editor.Dy() <= 0 {
			// Don't show cursor if editor is not visible
			return nil
		}
		if m.detailsOpen && m.isCompact {
			// Don't show cursor if details overlay is open
			return nil
		}

		if m.activeInline != nil {
			if cur := m.inlineCursor; cur != nil {
				cur.X++                        // Adjust for app margins
				cur.Y += m.layout.editor.Min.Y // Inline editor draws from area top
				return cur
			}
			return nil
		}

		if m.editor.textarea.Focused() {
			cur := m.editor.textarea.Cursor()
			cur.X++ // Adjust for app margins
			cur.Y += m.layout.editor.Min.Y + m.editorAttachmentsRowOffset()
			return cur
		}
	}
	return nil
}

func (m *UI) drawChatSeparators(scr uv.Screen, editorArea uv.Rectangle) {
	if editorArea.Dx() <= 0 {
		return
	}
	separator := m.com.Styles.Messages.ChatSeparator.Render(
		strings.Repeat(styles.SectionSeparator, editorArea.Dx()),
	)
	for _, y := range []int{editorArea.Min.Y - 1, editorArea.Max.Y} {
		if y < scr.Bounds().Min.Y || y >= scr.Bounds().Max.Y {
			continue
		}
		uv.NewStyledString(separator).Draw(scr, image.Rect(editorArea.Min.X, y, editorArea.Max.X, y+1))
	}
}

// View renders the UI model's view.
func (m *UI) View() tea.View {
	var v tea.View
	v.AltScreen = true
	if !m.isTransparent {
		v.BackgroundColor = m.com.Styles.Background
	}
	v.MouseMode = tea.MouseModeAllMotion
	v.ReportFocus = m.caps.ReportFocusEvents
	v.WindowTitle = "braid " + home.Short(m.com.Workspace.WorkingDir())
	if m.hasSession() && m.session.Title != "" {
		v.WindowTitle += " — " + m.session.Title
	}

	canvas := uv.NewScreenBuffer(m.width, m.height)
	v.Cursor = m.Draw(canvas, canvas.Bounds())

	content := strings.ReplaceAll(canvas.Render(), "\r\n", "\n") // normalize newlines
	contentLines := strings.Split(content, "\n")
	for i, line := range contentLines {
		// Trim trailing spaces for concise rendering
		contentLines[i] = strings.TrimRight(line, " ")
	}

	content = strings.Join(contentLines, "\n")

	v.Content = content
	if m.progressBarEnabled && m.sendProgressBar && m.isAgentBusy() {
		// HACK: use a random percentage to prevent ghostty from hiding it
		// after a timeout.
		v.ProgressBar = tea.NewProgressBar(tea.ProgressBarIndeterminate, rand.Intn(100))
	}

	return v
}

// ShortHelp implements [help.KeyMap].
func (m *UI) ShortHelp() []key.Binding {
	var binds []key.Binding
	k := &m.keyMap

	// When an inline editor is active, show its help.
	if m.activeInline != nil {
		return m.activeInline.ShortHelp()
	}

	commands := k.Commands
	if m.focus == uiFocusEditor && m.editor.textarea.Value() == "" {
		commands.SetHelp("/ or "+bindingShortcut(k.Commands), "commands")
	}

	switch m.state {
	case uiInitialize:
		binds = append(binds, k.Quit)
	case uiChat:
		// Show cancel binding if agent is busy.
		if m.isAgentBusy() {
			cancelBinding := k.Chat.Cancel
			if m.isCanceling {
				cancelBinding.SetHelp("esc", "press again to cancel")
			} else if m.wsCache.promptQueue > 0 {
				cancelBinding.SetHelp("esc", "clear queue")
			}
			binds = append(binds, cancelBinding)
		}

		// Show child-session navigation regardless of focus: the point is
		// discoverability of how to get back to the parent.
		if m.viewingChildSession() {
			binds = append(binds, k.Chat.ExitChildSession)
			if m.childSessionSiblingCount() > 1 {
				binds = append(binds, k.Chat.PrevChildSession, k.Chat.NextChildSession)
			}
		}

		binds = append(
			binds,
			commands,
			k.Models,
		)

		switch m.focus {
		case uiFocusEditor:
			binds = append(
				binds,
				k.Editor.Newline,
			)
		case uiFocusMain:
			binds = append(
				binds,
				k.Chat.UpDown,
				k.Chat.UpDownOneItem,
				k.Chat.PageUp,
				k.Chat.PageDown,
				k.Chat.Copy,
			)
			if _, _, ok := m.chat.SelectedNestedToolContainer(); ok {
				binds = append(binds, k.Chat.EnterChildSession)
			}
		}
	default:
		// TODO: other states
		// if m.session == nil {
		// no session selected
		binds = append(
			binds,
			commands,
			k.Models,
			k.Editor.Newline,
		)
	}

	binds = append(
		binds,
		k.Quit,
		k.Help,
	)

	return binds
}

// FullHelp implements [help.KeyMap].
func (m *UI) FullHelp() [][]key.Binding {
	// When an inline editor is active, show its help.
	if m.activeInline != nil {
		return [][]key.Binding{m.activeInline.ShortHelp()}
	}

	var binds [][]key.Binding
	k := &m.keyMap
	help := k.Help
	help.SetHelp(bindingShortcut(k.Help), "less")
	hasAttachments := len(m.editor.attachments.List()) > 0
	hasSession := m.hasSession()
	commands := k.Commands
	if m.focus == uiFocusEditor && m.editor.textarea.Value() == "" {
		commands.SetHelp("/ or "+bindingShortcut(k.Commands), "commands")
	}

	switch m.state {
	case uiInitialize:
		binds = append(binds,
			[]key.Binding{
				k.Quit,
			})
	case uiChat:
		// Show cancel binding if agent is busy.
		if m.isAgentBusy() {
			cancelBinding := k.Chat.Cancel
			if m.isCanceling {
				cancelBinding.SetHelp("esc", "press again to cancel")
			} else if m.wsCache.promptQueue > 0 {
				cancelBinding.SetHelp("esc", "clear queue")
			}
			binds = append(binds, []key.Binding{cancelBinding})
		}

		mainBinds := []key.Binding{}
		mainBinds = append(
			mainBinds,
			commands,
			k.Models,
			k.Sessions,
			k.ToggleYolo,
		)
		if hasSession {
			mainBinds = append(mainBinds, k.Chat.NewSession)
		}

		binds = append(binds, mainBinds)

		// Show child-session navigation regardless of focus: the point is
		// discoverability of how to get back to the parent.
		if m.viewingChildSession() {
			childBinds := []key.Binding{k.Chat.ExitChildSession}
			if m.childSessionSiblingCount() > 1 {
				childBinds = append(childBinds, k.Chat.PrevChildSession, k.Chat.NextChildSession)
			}
			binds = append(binds, childBinds)
		}

		switch m.focus {
		case uiFocusEditor:
			editorBinds := []key.Binding{
				k.Editor.Newline,
				k.Editor.MentionFile,
				k.Editor.Commands,
				k.Editor.OpenEditor,
			}
			if m.currentModelSupportsImages() {
				editorBinds = append(editorBinds, k.Editor.AddImage, k.Editor.PasteImage)
			}
			binds = append(binds, editorBinds)
			if hasAttachments {
				binds = append(
					binds,
					[]key.Binding{
						k.Editor.AttachmentDeleteMode,
						k.Editor.DeleteAllAttachments,
						k.Editor.Escape,
					},
				)
			}
		case uiFocusMain:
			binds = append(
				binds,
				[]key.Binding{
					k.Chat.UpDown,
					k.Chat.UpDownOneItem,
					k.Chat.PageUp,
					k.Chat.PageDown,
				},
				[]key.Binding{
					k.Chat.HalfPageUp,
					k.Chat.HalfPageDown,
					k.Chat.Home,
					k.Chat.End,
				},
				[]key.Binding{
					k.Chat.Copy,
					k.Chat.ClearHighlight,
				},
			)
			if _, _, ok := m.chat.SelectedNestedToolContainer(); ok {
				binds = append(binds, []key.Binding{k.Chat.EnterChildSession})
			}
		}
	default:
		if m.session == nil {
			// no session selected
			binds = append(
				binds,
				[]key.Binding{
					commands,
					k.Models,
					k.Sessions,
					k.ToggleYolo,
				},
			)
			editorBinds := []key.Binding{
				k.Editor.Newline,
				k.Editor.MentionFile,
				k.Editor.Commands,
				k.Editor.OpenEditor,
			}
			if m.currentModelSupportsImages() {
				editorBinds = append(editorBinds, k.Editor.AddImage, k.Editor.PasteImage)
			}
			binds = append(binds, editorBinds)
			if hasAttachments {
				binds = append(
					binds,
					[]key.Binding{
						k.Editor.AttachmentDeleteMode,
						k.Editor.DeleteAllAttachments,
						k.Editor.Escape,
					},
				)
			}
		}
	}

	binds = append(
		binds,
		[]key.Binding{
			help,
			k.Quit,
		},
	)

	return binds
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
	model := cfg.GetModelByType(config.SelectedModelTypeLarge)
	return model != nil && model.SupportsImages
}

// toggleCompactMode toggles compact mode between uiChat and uiChatCompact states.
// The actual SetCompactMode I/O runs inside the returned cmd; the UI state
// is updated only when the result lands via compactModeToggledMsg.
func (m *UI) toggleCompactMode() tea.Cmd {
	if m.compactModeLoading {
		return util.ReportWarn("Compact mode is already being updated")
	}
	desired := !m.forceCompactMode
	m.compactModeLoading = true
	m.compactModeGeneration++
	generation := m.compactModeGeneration
	workspace := m.com.Workspace
	return func() tea.Msg {
		return compactModeToggledMsg{Err: workspace.SetCompactMode(config.ScopeGlobal, desired), Enabled: desired, generation: generation}
	}
}

// updateLayoutAndSize updates the layout and sizes of UI components.
func (m *UI) updateLayoutAndSize() {
	// Determine if we should be in compact mode
	if m.state == uiChat {
		if m.forceCompactMode {
			m.isCompact = true
		} else if m.width < compactModeWidthBreakpoint || m.height < compactModeHeightBreakpoint {
			m.isCompact = true
		} else {
			m.isCompact = false
		}
	}

	// First pass sizes components from the current textarea height.
	m.layout = m.generateLayout(m.width, m.height)
	prevHeight := m.editor.textarea.Height()
	m.updateSize()

	// SetWidth can change textarea height due to soft-wrap recalculation.
	// If that happens, run one reconciliation pass with the new height.
	if m.editor.textarea.Height() != prevHeight {
		m.layout = m.generateLayout(m.width, m.height)
		m.updateSize()
	}
}

// handleTextareaHeightChange checks whether the textarea height changed and,
// if so, recalculates the layout. When the chat is in follow mode it keeps
// the view scrolled to the bottom. The returned command, if non-nil, must be
// batched by the caller.
func (m *UI) handleTextareaHeightChange(prevHeight int) tea.Cmd {
	if m.editor.textarea.Height() == prevHeight {
		return nil
	}
	m.updateLayoutAndSize()
	if m.state == uiChat && m.chat.Follow() {
		return m.chat.ScrollToBottomAndAnimate()
	}
	return nil
}

// updateTextarea updates the textarea for msg and then reconciles layout if
// the textarea height changed as a result.
func (m *UI) updateTextarea(msg tea.Msg) tea.Cmd {
	return m.updateTextareaWithPrevHeight(msg, m.editor.textarea.Height())
}

// updateTextareaWithPrevHeight is for cases when the height of the layout may
// have changed.
//
// Particularly, it's for cases where the textarea changes before
// textarea.Update is called (for example, SetValue, Reset, and InsertRune). We
// pass the height from before those changes took place so we can compare
// "before" vs "after" sizing and recalculate the layout if the textarea grew
// or shrank.
func (m *UI) updateTextareaWithPrevHeight(msg tea.Msg, prevHeight int) tea.Cmd {
	ta, cmd := m.editor.textarea.Update(msg)
	m.editor.textarea = ta
	return tea.Batch(cmd, m.handleTextareaHeightChange(prevHeight))
}

// updateSize updates the sizes of UI components based on the current layout.
func (m *UI) updateSize() {
	// Set status width
	m.status.SetWidth(m.layout.status.Dx())

	m.chat.SetSize(m.layout.main.Dx(), m.layout.main.Dy())
	m.editor.textarea.MaxHeight = TextareaMaxHeight
	m.editor.textarea.SetWidth(m.layout.editor.Dx())

	// Handle different app states
	switch m.state {
	case uiChat:
		if !m.isCompact {
			m.cacheSidebarLogo(m.layout.sidebar.Dx())
		}
	}
}

// generateLayout calculates the layout rectangles for all UI components based
// on the current UI state and terminal dimensions.
func (m *UI) generateLayout(w, h int) uiLayout {
	// The screen area we're working with
	area := image.Rect(0, 0, w, h)

	// The help height
	helpHeight := 1
	// The editor height: textarea height, plus one row for the attachments
	// strip when there are attachments to show. No fixed bottom margin — a
	// one-line, no-attachments editor is exactly one row.
	// When an inline editor is active, use its height instead.
	editorHeight := m.editor.textarea.Height() + m.editorAttachmentsRowOffset()
	if m.activeInline != nil {
		// The editor content width depends only on terminal width
		// and layout (not on editor height), so passing the current
		// frame's width to Height() keeps layout in sync with the
		// width Draw will use, preventing flicker during fast resize.
		editorWidth := m.editorContentWidth()
		if m.focus == uiFocusEditor {
			editorHeight = m.activeInline.Height(editorWidth)
		} else if qf, ok := m.activeInline.(*dialog.QuestionForm); ok && m.shouldCollapseQuestion(qf) {
			editorHeight = qf.CollapsedHeight() + 1
		} else {
			editorHeight = m.activeInline.Height(editorWidth)
		}
	} else if m.viewingChildSession() {
		// The child-session info panel (drawChildSessionPanel) replaces the
		// textarea entirely while viewing a sub-agent session — it has its
		// own fixed, compact height instead of the textarea's.
		editorHeight = childSessionPanelHeight
	}
	// The sidebar width
	sidebarWidth := 32
	// The header height
	const landingHeaderHeight = 4

	var helpKeyMap help.KeyMap = m
	if m.status != nil && m.status.ShowingAll() {
		for _, row := range helpKeyMap.FullHelp() {
			helpHeight = max(helpHeight, len(row))
		}
	}

	// Add app margins
	var appRect, helpRect image.Rectangle
	layout.Vertical(
		layout.Len(area.Dy()-helpHeight),
		layout.Fill(1),
	).Split(area).Assign(&appRect, &helpRect)
	appRect.Min.Y += 1
	appRect.Max.Y -= 1
	helpRect.Min.Y -= 1
	appRect.Min.X += 1
	appRect.Max.X -= 1

	if slices.Contains([]uiState{uiOnboarding, uiInitialize, uiLanding}, m.state) {
		// extra padding on left and right for these states
		appRect.Min.X += 1
		appRect.Max.X -= 1
	}

	uiLayout := uiLayout{
		area:   area,
		status: helpRect,
	}

	// Handle different app states
	switch m.state {
	case uiOnboarding, uiInitialize:
		// Layout
		//
		// header
		// ------
		// main
		// ------
		// help

		var headerRect, mainRect image.Rectangle
		layout.Vertical(
			layout.Len(landingHeaderHeight),
			layout.Fill(1),
		).Split(appRect).Assign(&headerRect, &mainRect)
		uiLayout.header = headerRect
		uiLayout.main = mainRect

	case uiLanding:
		// Layout
		//
		// header
		// ------
		// main
		// ------
		// editor
		// ------
		// help
		var headerRect, mainRect image.Rectangle
		layout.Vertical(
			layout.Len(landingHeaderHeight),
			layout.Fill(1),
		).Split(appRect).Assign(&headerRect, &mainRect)
		var editorRect image.Rectangle
		layout.Vertical(
			layout.Len(mainRect.Dy()-editorHeight),
			layout.Fill(1),
		).Split(mainRect).Assign(&mainRect, &editorRect)
		// Remove extra padding from editor (but keep it for header and main)
		editorRect.Min.X -= 1
		editorRect.Max.X += 1
		uiLayout.header = headerRect
		uiLayout.main = mainRect
		uiLayout.editor = editorRect

	case uiChat:
		if m.isCompact {
			// Layout
			//
			// compact-header
			// ------
			// main
			// ------
			// editor
			// ------
			// help
			const compactHeaderHeight = 1
			var headerRect, mainRect image.Rectangle
			layout.Vertical(
				layout.Len(compactHeaderHeight),
				layout.Fill(1),
			).Split(appRect).Assign(&headerRect, &mainRect)
			detailsHeight := min(sessionDetailsMaxHeight, area.Dy()-1) // One row for the header
			var sessionDetailsArea image.Rectangle
			layout.Vertical(
				layout.Len(detailsHeight),
				layout.Fill(1),
			).Split(appRect).Assign(&sessionDetailsArea, new(image.Rectangle))
			uiLayout.sessionDetails = sessionDetailsArea
			uiLayout.sessionDetails.Min.Y += compactHeaderHeight // adjust for header
			// Add one line gap between header and main content
			mainRect.Min.Y += 1
			var editorRect image.Rectangle
			layout.Vertical(
				layout.Len(max(0, mainRect.Dy()-editorHeight-2)),
				layout.Len(1),
				layout.Len(editorHeight),
				layout.Len(1),
			).Split(mainRect).Assign(&mainRect, new(image.Rectangle), &editorRect, new(image.Rectangle))
			mainRect.Max.X -= 1 // Add padding right
			uiLayout.header = headerRect
			panelHeight := m.sessionPanelHeight(mainRect.Dy() + 2)
			if panelHeight > 0 {
				var chatRect, panelRect image.Rectangle
				layout.Vertical(
					layout.Len(mainRect.Dy()-panelHeight),
					layout.Fill(1),
				).Split(mainRect).Assign(&chatRect, &panelRect)
				uiLayout.main = chatRect
				uiLayout.panel = panelRect
			} else {
				uiLayout.main = mainRect
				uiLayout.panel = image.Rectangle{}
			}
			uiLayout.editor = editorRect
		} else {
			// Layout
			//
			// ------|---
			// main  |
			// ------| side
			// editor|
			// ----------
			// help

			var mainRect, sideRect image.Rectangle
			layout.Horizontal(
				layout.Len(appRect.Dx()-sidebarWidth),
				layout.Fill(1),
			).Split(appRect).Assign(&mainRect, &sideRect)
			// Add padding left
			sideRect.Min.X += 1
			var editorRect image.Rectangle
			layout.Vertical(
				layout.Len(max(0, mainRect.Dy()-editorHeight-2)),
				layout.Len(1),
				layout.Len(editorHeight),
				layout.Len(1),
			).Split(mainRect).Assign(&mainRect, new(image.Rectangle), &editorRect, new(image.Rectangle))
			mainRect.Max.X -= 1 // Add padding right
			uiLayout.sidebar = sideRect
			panelHeight := m.sessionPanelHeight(mainRect.Dy() + 2)
			if panelHeight > 0 {
				var chatRect, panelRect image.Rectangle
				layout.Vertical(
					layout.Len(mainRect.Dy()-panelHeight),
					layout.Fill(1),
				).Split(mainRect).Assign(&chatRect, &panelRect)
				uiLayout.main = chatRect
				uiLayout.panel = panelRect
			} else {
				uiLayout.main = mainRect
				uiLayout.panel = image.Rectangle{}
			}
			uiLayout.editor = editorRect
		}
	}

	return uiLayout
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

func (m *UI) openEditor(value string) tea.Cmd {
	tmpfile, err := os.CreateTemp("", "msg_*.md")
	if err != nil {
		return util.ReportError(err)
	}
	tmpPath := tmpfile.Name()
	defer tmpfile.Close()
	if _, err := tmpfile.WriteString(value); err != nil {
		return util.ReportError(err)
	}
	cmd, err := editor.Command(
		"braid",
		tmpPath,
		editor.AtPosition(
			m.editor.textarea.Line()+1,
			m.editor.textarea.Column()+1,
		),
	)
	if err != nil {
		return util.ReportError(err)
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer func() {
			_ = os.Remove(tmpPath)
		}()

		if err != nil {
			return util.ReportError(err)
		}
		content, err := os.ReadFile(tmpPath)
		if err != nil {
			return util.ReportError(err)
		}
		if len(content) == 0 {
			return util.ReportWarn("Message is empty")
		}
		return openEditorMsg{
			Text: strings.TrimSpace(string(content)),
		}
	})
}

// setEditorPrompt configures the textarea prompt function based on whether
// yolo mode or bang mode is enabled.
func (m *UI) setEditorPrompt(yolo bool) {
	if m.editor.bangMode {
		m.editor.textarea.SetPromptFunc(4, m.bangPromptFunc)
		return
	}
	if yolo {
		m.editor.textarea.SetPromptFunc(4, m.yoloPromptFunc)
		return
	}
	m.editor.textarea.SetPromptFunc(2, m.normalPromptFunc)
}

// normalPromptFunc keeps the prompt width as whitespace so multiline text
// stays aligned without visible prompt markers.
func (m *UI) normalPromptFunc(info textarea.PromptInfo) string {
	return "  "
}

// yoloPromptFunc returns the yolo mode editor prompt style with warning icon
// and colored dots.
func (m *UI) yoloPromptFunc(info textarea.PromptInfo) string {
	t := m.com.Styles
	if info.LineNumber == 0 {
		if info.Focused {
			return t.Editor.PromptYoloIconFocused.Render()
		} else {
			return t.Editor.PromptYoloIconBlurred.Render()
		}
	}
	return "    "
}

// bangPromptFunc returns the bang mode editor prompt style with Turtle-colored
// icon and dots.
func (m *UI) bangPromptFunc(info textarea.PromptInfo) string {
	t := m.com.Styles
	if info.LineNumber == 0 {
		if info.Focused {
			return t.Editor.PromptBangIconFocused.Render()
		}
		return t.Editor.PromptBangIconBlurred.Render()
	}
	return "    "
}

// insertFileCompletion inserts the selected file path into the textarea,
// replacing the @query, and adds the file as an attachment.
func (m *UI) insertFileCompletion(path string) tea.Cmd {
	prevHeight := m.editor.textarea.Height()
	if !m.editor.insertCompletionText(path) {
		return nil
	}
	heightCmd := m.handleTextareaHeightChange(prevHeight)

	fileCmd := func() tea.Msg {
		absPath, _ := filepath.Abs(path)

		if m.hasSession() {
			// Skip attachment if file was already read and hasn't been modified.
			lastRead := m.com.Workspace.FileTrackerLastReadTime(context.Background(), m.session.ID, absPath)
			if !lastRead.IsZero() {
				if info, err := os.Stat(path); err == nil && !info.ModTime().After(lastRead) {
					return nil
				}
			}
		} else if slices.Contains(m.sessionFileReads, absPath) {
			return nil
		}

		m.sessionFileReads = append(m.sessionFileReads, absPath)

		// Add file as attachment.
		content, err := os.ReadFile(path)
		if err != nil {
			// If it fails, let the LLM handle it later.
			return nil
		}

		return message.Attachment{
			FilePath: path,
			FileName: filepath.Base(path),
			MimeType: mimeOf(content),
			Content:  content,
		}
	}
	return tea.Batch(heightCmd, fileCmd)
}

// insertMCPResourceCompletion inserts the selected resource into the textarea,
// replacing the @query, and adds the resource as an attachment.
func (m *UI) insertMCPResourceCompletion(item completions.ResourceCompletionValue) tea.Cmd {
	displayText := cmp.Or(item.Title, item.URI)

	prevHeight := m.editor.textarea.Height()
	if !m.editor.insertCompletionText(displayText) {
		return nil
	}
	heightCmd := m.handleTextareaHeightChange(prevHeight)

	resourceCmd := func() tea.Msg {
		contents, err := m.com.Workspace.ReadMCPResource(
			context.Background(),
			item.MCPName,
			item.URI,
		)
		if err != nil {
			slog.Warn("Failed to read MCP resource", "uri", item.URI, "error", err)
			return nil
		}
		if len(contents) == 0 {
			return nil
		}

		content := contents[0]
		var data []byte
		if content.Text != "" {
			data = []byte(content.Text)
		} else if len(content.Blob) > 0 {
			data = content.Blob
		}
		if len(data) == 0 {
			return nil
		}

		mimeType := item.MIMEType
		if mimeType == "" && content.MIMEType != "" {
			mimeType = content.MIMEType
		}
		if mimeType == "" {
			mimeType = "text/plain"
		}

		return message.Attachment{
			FilePath: item.URI,
			FileName: displayText,
			MimeType: mimeType,
			Content:  data,
		}
	}
	return tea.Batch(heightCmd, resourceCmd)
}

// activeGhostTail returns the acceptable/renderable suffix of the current
// history-based ghost prediction for the editor, or "" if none should be
// shown right now (wrong focus, completions open, bang mode, empty input,
// no match, or hidden by a preceding Esc).
func (m *UI) activeGhostTail() string {
	if m.focus != uiFocusEditor || m.editor.completionsOpen || m.editor.bangMode {
		return ""
	}
	value := m.editor.textarea.Value()
	if value == "" {
		return ""
	}
	full := m.editor.ghostSuggestionFor(value)
	if full == "" || m.editor.ghostHiddenFor == value {
		return ""
	}
	return full[len(value):]
}

// drawGhostText overlays the inline history prediction (if any) onto the
// editor at the cursor position, on top of the already-drawn textarea. It
// is a pure overlay: it only paints the cells for the predicted text and
// never affects layout/height.
func (m *UI) drawGhostText(scr uv.Screen) {
	tail := m.activeGhostTail()
	if tail == "" {
		return
	}
	cur := m.editor.textarea.Cursor()
	if cur == nil {
		return
	}
	// Same cursor-to-screen mapping as completionsPosition(): textarea
	// cursor coordinates offset by the editor layout rect's origin, plus
	// editorAttachmentsRowOffset() for the attachments line renderEditorView
	// conditionally prepends above the textarea (mirrors the offset applied
	// where the completions popup position is used, above).
	x := m.layout.editor.Min.X + cur.X
	y := m.layout.editor.Min.Y + cur.Y + m.editorAttachmentsRowOffset()
	if x >= m.layout.editor.Max.X || y >= m.layout.editor.Max.Y {
		return
	}

	// Only the first line of a multi-line prediction is shown, with a
	// trailing "…" marker to indicate there's more; Tab still accepts the
	// full multi-line tail regardless of what's rendered here.
	display := tail
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 {
		display = tail[:idx] + "…"
	}

	ghost := uv.NewStyledString(m.com.Styles.Editor.Textarea.Focused.Placeholder.Render(display))
	ghost.Tail = "…" // mark truncation if it overflows the editor width
	ghost.Draw(scr, image.Rectangle{
		Min: image.Pt(x, y),
		Max: image.Pt(m.layout.editor.Max.X, y+1),
	})
}

// completionsPosition returns the X and Y position for the completions popup.
func (m *UI) completionsPosition() image.Point {
	cur := m.editor.textarea.Cursor()
	if cur == nil {
		return image.Point{
			X: m.layout.editor.Min.X,
			Y: m.layout.editor.Min.Y,
		}
	}
	return image.Point{
		X: cur.X + m.layout.editor.Min.X,
		Y: m.layout.editor.Min.Y + cur.Y,
	}
}

// isWhitespace returns true if the byte is a whitespace character.
func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// isAgentBusy returns true if the agent coordinator exists and is currently
// busy processing a request. It only reads the memoized state (it runs in
// per-message paths like the textarea placeholder, where a workspace probe
// would be an HTTP round-trip per keystroke in client/server mode); the
// value is refreshed off-thread, see workspace_cache.go.
func (m *UI) isAgentBusy() bool {
	if m.editor.bangCancel != nil {
		return true
	}
	return m.wsCache.agentBusyCache.val
}

// hasSession returns true if there is an active session with a valid ID.
func (m *UI) hasSession() bool {
	return m.session != nil && m.session.ID != ""
}

// mimeOf detects the MIME type of the given content.
func mimeOf(content []byte) string {
	mimeBufferSize := min(512, len(content))
	return http.DetectContentType(content[:mimeBufferSize])
}

var readyPlaceholders = [...]string{
	"Ready!",
	"Ready...",
	"Ready?",
	"Ready for instructions",
}

var workingPlaceholders = [...]string{
	"Working!",
	"Working...",
	"Brrrrr...",
	"Prrrrrrrr...",
	"Processing...",
	"Thinking...",
}

// randomizePlaceholders selects random placeholder text for the textarea's
// ready and working states.
func (m *UI) randomizePlaceholders() {
	m.workingPlaceholder = workingPlaceholders[rand.Intn(len(workingPlaceholders))]
	m.readyPlaceholder = readyPlaceholders[rand.Intn(len(readyPlaceholders))]
}

// renderEditorView renders the editor view with attachments if any. The
// attachments strip only takes a row when there's something to show — an
// empty editor with a single line of text is exactly one row tall, with no
// reserved blank lines above or below (see editorAttachmentsRowOffset and
// TextareaMinHeight).
func (m *UI) renderEditorView(width int) string {
	if len(m.editor.attachments.List()) == 0 {
		return m.editor.textarea.View()
	}
	return strings.Join([]string{
		m.editor.attachments.Render(width),
		m.editor.textarea.View(),
	}, "\n")
}

// editorAttachmentsRowOffset returns the number of rows renderEditorView
// prepends above the textarea for the attachments strip: 1 when there are
// attachments to show, 0 otherwise. Every consumer that maps a textarea-
// relative coordinate (cursor, ghost text, completions popup) onto the
// editor's screen rect must add this, since renderEditorView's attachments
// row is conditional rather than a fixed margin.
func (m *UI) editorAttachmentsRowOffset() int {
	// generateLayout (via updateLayoutAndSize) can run before
	// editor.attachments is wired up in some construction paths (notably
	// test harnesses that only need layout, not the full editor), so this
	// stays nil-safe unlike the Draw-time attachments call sites below,
	// which only ever run once the editor is fully set up.
	if m.editor.attachments != nil && len(m.editor.attachments.List()) > 0 {
		return editorAttachmentsRowHeight
	}
	return 0
}

// cacheSidebarLogo renders and caches the sidebar logo at the specified width.
func (m *UI) cacheSidebarLogo(width int) {
	m.sidebar.logo = renderLogo(m.com.Styles, true, width)
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

// sendMessage sends a message with the given content and attachments.
// All I/O (AgentReadyErr, CreateSession, AgentRun) runs inside a tea.Cmd
// so that the Update goroutine is never blocked.
func (m *UI) sendMessage(content string, attachments ...message.Attachment) tea.Cmd {
	if m.viewingChildSession() {
		return util.ReportWarn("viewing subagent session · " + m.exitChildSessionShortcut() + " to return")
	}
	if m.session != nil && m.sessionLoadExpectedID != "" && m.sessionLoadExpectedID != m.session.ID {
		m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{
			content:        content,
			attachments:    attachments,
			sessionID:      m.sessionLoadExpectedID,
			loadGeneration: m.sessionLoadGen,
		})
		return nil
	}
	if m.session != nil {
		if m.editor.pendingSendActive {
			m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{
				content:        content,
				attachments:    attachments,
				sessionID:      m.session.ID,
				loadGeneration: m.sessionLoadGen,
			})
			return nil
		}
		m.editor.pendingSendActive = true
	}
	return m.sendMessageNow(content, attachments...)
}

func (m *UI) sendMessageNow(content string, attachments ...message.Attachment) tea.Cmd {
	if m.session == nil && m.editor.pendingSendLoading {
		m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{content: content, attachments: attachments, generation: m.editor.pendingSendGen})
		return nil
	}

	workspace := m.com.Workspace
	styles := m.com.Styles
	reads := append([]string(nil), m.sessionFileReads...)
	ctx := context.Background()
	sessionID := ""
	generation := m.editor.pendingSendGen
	loadGeneration := m.sessionLoadGen
	creating := m.session == nil
	if creating {
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen++
		generation = m.editor.pendingSendGen
	} else {
		sessionID = m.session.ID
		m.wsCache.agentBusyCache.set(true)
		m.wsCache.busyFetchGen++
		m.invalidatePromptQueue()
	}

	return func() tea.Msg {
		if err := workspace.AgentReadyErr(); err != nil {
			return sendMessageErrorMsg{Err: err, generation: generation, sessionID: sessionID, loadGeneration: loadGeneration, creating: creating}
		}
		if creating {
			created, err := workspace.CreateSession(ctx, "New Session")
			if err != nil {
				return sendMessageErrorMsg{Err: err, generation: generation, creating: true}
			}
			return createSessionMsg{session: created, content: content, attachments: attachments, generation: generation}
		}
		common.StartTurn()
		for _, path := range reads {
			workspace.FileTrackerRecordRead(ctx, sessionID, path)
			workspace.LSPStart(ctx, path)
		}
		if err := workspace.AgentRun(ctx, sessionID, content, attachments...); err != nil && !errors.Is(err, context.Canceled) {
			var quotaErr *agent.ProviderQuotaError
			if errors.As(err, &quotaErr) {
				link := styles.Dialog.OAuth.Link.Hyperlink(quotaErr.SettingsURL, "id=copilot").Render(quotaErr.SettingsURL)
				return sendMessageErrorMsg{Err: fmt.Errorf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", quotaErr.Model, link), generation: generation, sessionID: sessionID, loadGeneration: loadGeneration}
			}
			return sendMessageErrorMsg{Err: err, generation: generation, sessionID: sessionID, loadGeneration: loadGeneration}
		}
		return agentRunSubmittedMsg{sessionID: sessionID, loadGeneration: loadGeneration}
	}
}

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

// runShellCommand executes a shell command server-side without triggering
// the LLM. The result is displayed as a tool-style item in the chat.
func (m *UI) runShellCommand(command string) tea.Cmd {
	if m.viewingChildSession() {
		return util.ReportWarn("viewing subagent session · " + m.exitChildSessionShortcut() + " to return")
	}
	if m.session != nil {
		m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{
			content:        command,
			sessionID:      m.session.ID,
			loadGeneration: m.sessionLoadGen,
			bang:           true,
		})
		return func() tea.Msg { return sendPendingQueueMsg{} }
	}
	return m.runShellCommandInternal(command, true)
}

// runShellCommandInternal is the shared implementation for bang-mode shell
// execution. isFirstMessage indicates the command is the first user message
// in a newly created session, which triggers title generation.
func (m *UI) runShellCommandInternal(command string, isFirstMessage bool) tea.Cmd {
	var cmds []tea.Cmd
	if !m.hasSession() {
		if m.editor.pendingSendLoading {
			m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{content: command, generation: m.editor.pendingSendGen, bang: true})
			return nil
		}
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen++
		generation := m.editor.pendingSendGen
		workspace := m.com.Workspace
		cmds = append(cmds, func() tea.Msg {
			newSession, err := workspace.CreateSession(context.Background(), "New Session")
			if err != nil {
				return sendMessageErrorMsg{Err: err, generation: generation, creating: true}
			}
			return bangSessionCreatedMsg{session: newSession, command: command, isFirstMessage: isFirstMessage, generation: generation}
		})
		return tea.Batch(cmds...)
	}

	sessionID := m.session.ID
	loadGeneration := m.sessionLoadGen
	contentWidth := min(m.layout.main.Dx()-2, 120)

	// Append a pending shell item immediately so the user sees feedback.
	pendingItem := chat.NewPendingShellItem(m.com.Styles, command)
	m.chat.AppendMessages(pendingItem)
	if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := pendingItem.StartAnimation(); cmd != nil {
		cmds = append(cmds, cmd)
	}

	// Stream output via channel. The progress callback writes chunks
	// to streamCh; a reader cmd converts them to shellStreamMsg values.
	streamCh := make(chan string, 64)
	pendingID := pendingItem.ID()

	onProgress := func(chunk string) {
		select {
		case streamCh <- chunk:
		default:
			// Drop if UI can't keep up.
		}
	}

	// Reader cmd: drains streamCh into shellStreamMsg until closed.
	cmds = append(cmds, func() tea.Msg {
		chunk, ok := <-streamCh
		if !ok {
			return nil
		}
		return shellStreamMsg{PendingID: pendingID, Chunk: chunk, streamCh: streamCh}
	})

	ctx, cancel := context.WithCancel(context.Background())
	m.editor.bangCancel = cancel

	workspace := m.com.Workspace
	cmds = append(cmds, func() tea.Msg {
		resp, err := workspace.AgentRunShellCommand(ctx, sessionID, command, contentWidth, onProgress, isFirstMessage)
		close(streamCh)
		result := shellResultMsg{
			PendingID:  pendingID,
			Command:    command,
			Output:     resp.Output,
			sessionID:  sessionID,
			generation: loadGeneration,
		}
		if errors.Is(err, context.Canceled) {
			result.Canceled = true
			result.ExitCode = 130
			return result
		}
		if err != nil {
			result.Err = err
			result.ExitCode = 1
			return result
		}
		result.ExitCode = resp.ExitCode
		return result
	})
	return tea.Batch(cmds...)
}

const cancelTimerDuration = 2 * time.Second

// cancelTimerCmd creates a command that expires the cancel timer.
func cancelTimerCmd() tea.Cmd {
	return tea.Tick(cancelTimerDuration, func(time.Time) tea.Msg {
		return cancelTimerExpiredMsg{}
	})
}

// cancelAgent handles the cancel key press. The first press sets isCanceling to true
// and starts a timer. The second press (before the timer expires) actually
// cancels the agent.
func (m *UI) cancelAgent() tea.Cmd {
	if !m.hasSession() {
		return nil
	}

	// Gate on the memoized ready state: esc is a hot key and AgentIsReady
	// is a synchronous HTTP round-trip in client/server mode.
	if !m.wsCache.agentReady {
		return nil
	}

	if m.isCanceling {
		// Second escape press — actually cancel.
		m.isCanceling = false

		// Cancel a running bang command if one is in progress.
		if m.editor.bangCancel != nil {
			m.editor.bangCancel()
			m.editor.bangCancel = nil
		}

		m.com.Workspace.AgentCancel(m.session.ID)
		// Stop the spinning todo indicator and drop the memoized busy
		// state the cancel just changed; the session panel reads
		// m.panelIsSpinning fresh on every draw, and again once the
		// off-thread refresh (and the agent's own events) land.
		m.panelIsSpinning = false
		m.invalidateBusyCaches()
		return m.dispatchBusyRefresh()
	}

	// Queued prompts pending: esc clears the queue. Decide from the cached
	// count (event-driven) instead of a synchronous workspace probe.
	if m.wsCache.promptQueue > 0 {
		m.com.Workspace.AgentClearQueue(m.session.ID)
		m.wsCache.promptQueue = 0
		m.wsCache.promptQueueItems = nil
		m.wsCache.promptQueueCheckedAt = time.Now()
		// Bump the queue generation so a fetch started before this clear
		// cannot land and repopulate the pill we just emptied.
		m.invalidatePromptQueue()
		m.updateLayoutAndSize()
		return nil
	}

	// First escape press - set canceling state and start timer.
	m.isCanceling = true
	return cancelTimerCmd()
}

// openDialog opens a dialog by its ID.
func (m *UI) openDialog(id string) tea.Cmd {
	var cmds []tea.Cmd
	switch id {
	case dialog.SessionsID:
		if cmd := m.openSessionsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ModelsID:
		if cmd := m.openModelsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.CommandsID:
		if cmd := m.openCommandsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ReasoningID:
		if cmd := m.openReasoningDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ProvidersID:
		if cmd := m.openProvidersDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.NotificationsID:
		if cmd := m.openNotificationsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.FilePickerID:
		if cmd := m.openFilesDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.QuitID:
		if cmd := m.openQuitDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.DoctorID:
		m.openDoctorDialog()
	default:
		// Unknown dialog
		break
	}
	return tea.Batch(cmds...)
}

// openQuitDialog opens the quit confirmation dialog.
func (m *UI) openQuitDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.QuitID) {
		// Bring to front
		m.dialog.BringToFront(dialog.QuitID)
		return nil
	}

	quitDialog := dialog.NewQuit(m.com)
	m.dialog.OpenDialog(quitDialog)
	return nil
}

// openModelsDialog opens the models dialog.
func (m *UI) openModelsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ModelsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.ModelsID)
		return nil
	}

	modelsDialog, err := dialog.NewModels(m.com)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(modelsDialog)

	return nil
}

// openCommandsDialog opens the commands palette dialog (ctrl+p): the
// mouse-friendly fallback for browsing commands. The editor's "/" trigger no
// longer opens this — see commandCompletionItems for that path — but both
// draw from the same command list via dialog.BuildCommandItems.
func (m *UI) openCommandsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.CommandsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.CommandsID)
		return nil
	}

	var sessionID string
	hasSession := m.session != nil
	if hasSession {
		sessionID = m.session.ID
	}
	hasTodos := hasSession && hasIncompleteTodos(m.session.Todos)
	hasQueue := m.wsCache.promptQueue > 0

	commands, err := dialog.NewCommands(m.com, sessionID, hasSession, hasTodos, hasQueue, m.customCommands, m.mcpPrompts)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(commands)

	return commands.InitialCmd()
}

// commandCompletionItems builds the flat command list for the editor's "/"
// completion popup, from the same dialog.BuildCommandItems provider the
// Commands palette dialog uses, so the two never list different commands.
func (m *UI) commandCompletionItems() []completions.CommandCompletionValue {
	var sessionID string
	hasSession := m.session != nil
	if hasSession {
		sessionID = m.session.ID
	}
	hasTodos := hasSession && hasIncompleteTodos(m.session.Todos)
	hasQueue := m.wsCache.promptQueue > 0

	var dockerMCPAvailable *bool
	if available, known := config.DockerMCPAvailabilityCached(); known {
		dockerMCPAvailable = &available
	}

	items := dialog.BuildCommandItems(m.com, sessionID, hasSession, hasTodos, hasQueue, m.width, dockerMCPAvailable, m.customCommands, m.mcpPrompts)
	values := make([]completions.CommandCompletionValue, 0, len(items))
	for _, item := range items {
		values = append(values, completions.CommandCompletionValue{
			ID:          item.ID(),
			Title:       item.Title(),
			Aliases:     item.Aliases(),
			Description: item.Description(),
			Shortcut:    item.Shortcut(),
			Action:      item.Action(),
		})
	}
	return values
}

// openReasoningDialog opens the reasoning effort dialog.
func (m *UI) openReasoningDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ReasoningID) {
		m.dialog.BringToFront(dialog.ReasoningID)
		return nil
	}

	reasoningDialog, err := dialog.NewReasoning(m.com)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(reasoningDialog)
	return nil
}

// openProvidersDialog opens the providers configuration dialog. It's the
// onboarding entry point (see Init) as well as reachable from the command
// palette — see the "configure_providers" entry in dialog/commands.go.
func (m *UI) openProvidersDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ProvidersID) {
		m.dialog.BringToFront(dialog.ProvidersID)
		return nil
	}

	isOnboarding := m.state == uiOnboarding
	providersDialog, err := dialog.NewProviders(m.com, isOnboarding)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(providersDialog)
	return nil
}

// openProviderFormDialog opens the custom provider form dialog.
func (m *UI) openProviderFormDialog() {
	if m.dialog.ContainsDialog(dialog.ProviderFormID) {
		m.dialog.BringToFront(dialog.ProviderFormID)
		return
	}

	formDialog := dialog.NewProviderForm(m.com)
	m.dialog.OpenDialog(formDialog)
}

// configureProvider resolves providerID to its catalog entry and opens the
// matching authentication dialog (OAuth for Copilot, API key input
// otherwise), mirroring openAuthenticationDialog's dispatch. The API key
// dialog is opened in its model-less mode (nil model) since this flow
// isn't switching a model, just authenticating a provider.
func (m *UI) configureProvider(providerID string) tea.Cmd {
	providers, err := config.Providers(m.com.Config())
	if err != nil && len(providers) == 0 {
		return util.ReportError(err)
	}

	idx := slices.IndexFunc(providers, func(p catwalk.Provider) bool {
		return string(p.ID) == providerID
	})
	if idx == -1 {
		return util.ReportError(fmt.Errorf("unknown provider %q", providerID))
	}
	provider := providers[idx]

	var (
		dlg dialog.Dialog
		cmd tea.Cmd

		isOnboarding = m.state == uiOnboarding
	)
	switch provider.ID {
	case catwalk.InferenceProviderCopilot:
		dlg, cmd = dialog.NewOAuthCopilot(m.com, isOnboarding, provider, nil, "")
	default:
		dlg, cmd = dialog.NewAPIKeyInput(m.com, isOnboarding, provider, nil, "")
	}

	if m.dialog.ContainsDialog(dlg.ID()) {
		m.dialog.BringToFront(dlg.ID())
		return nil
	}

	m.dialog.OpenDialogWithGrace(dlg)
	return cmd
}

// openNotificationsDialog opens the notification style picker dialog.
//
//nolint:unparam // always nil today, but matches the tea.Cmd signature shared by the other open*Dialog methods
func (m *UI) openNotificationsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.NotificationsID) {
		m.dialog.BringToFront(dialog.NotificationsID)
		return nil
	}

	notificationsDialog := dialog.NewNotifications(m.com)
	m.dialog.OpenDialog(notificationsDialog)
	return nil
}

// openDoctorDialog opens the /doctor config-problems dialog.
func (m *UI) openDoctorDialog() {
	if m.dialog.ContainsDialog(dialog.DoctorID) {
		m.dialog.BringToFront(dialog.DoctorID)
		return
	}

	m.dialog.OpenDialog(dialog.NewDoctor(m.com))
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

// openSessionsDialog opens the sessions dialog. If the dialog is already
// open, it brings it to the front. Otherwise it dispatches an off-thread
// ListSessions fetch (a synchronous HTTP round-trip in client/server mode)
// and opens the dialog once sessionsLoadedMsg lands; see
// applySessionsLoaded.
func (m *UI) openSessionsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.SessionsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.SessionsID)
		return nil
	}
	if m.sessionsDialogLoading {
		// A fetch is already in flight; don't stack another one.
		return nil
	}

	selectedSessionID := ""
	if m.session != nil {
		selectedSessionID = m.session.ID
	}

	m.sessionsDialogLoading = true
	m.sessionsDialogGen++
	gen := m.sessionsDialogGen
	ws := m.com.Workspace
	ctx := m.com.Context()
	return func() tea.Msg {
		sessions, err := ws.ListSessions(ctx)
		return sessionsLoadedMsg{
			gen:               gen,
			sessions:          sessions,
			selectedSessionID: selectedSessionID,
			err:               err,
		}
	}
}

// applySessionsLoaded opens the sessions dialog with the fetched list once
// it lands. A generation mismatch means a newer openSessionsDialog call
// superseded this fetch (e.g. the user pressed the key again before it
// landed), so the stale result is dropped instead of popping the dialog
// open unexpectedly.
func (m *UI) applySessionsLoaded(msg sessionsLoadedMsg) tea.Cmd {
	if msg.gen != m.sessionsDialogGen {
		return nil
	}
	m.sessionsDialogLoading = false
	if msg.err != nil {
		return util.ReportError(msg.err)
	}
	if m.dialog.ContainsDialog(dialog.SessionsID) {
		return nil
	}
	m.dialog.OpenDialog(dialog.NewSessions(m.com, msg.sessions, msg.selectedSessionID))
	return nil
}

// openFilesDialog opens the file picker dialog.
func (m *UI) openFilesDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.FilePickerID) {
		// Bring to front
		m.dialog.BringToFront(dialog.FilePickerID)
		return nil
	}

	filePicker, cmd := dialog.NewFilePicker(m.com)
	filePicker.SetImageCapabilities(&m.caps)
	m.dialog.OpenDialog(filePicker)
	event.FilePickerOpened()

	return cmd
}

// openPermissionsDialog opens the permissions dialog for a permission request.
//
//nolint:unparam // always nil today, but matches the tea.Cmd signature shared by the other open*Dialog methods
func (m *UI) openPermissionsDialog(perm permission.PermissionRequest) tea.Cmd {
	m.permissionGeneration++
	m.permissionLoading = false
	m.permissionID = perm.ID
	// Close any existing permissions dialog first.
	m.dialog.CloseDialog(dialog.PermissionsID)

	// Get diff mode from config.
	var opts []dialog.PermissionsOption
	if diffMode := m.com.Config().Options.TUI.DiffMode; diffMode != "" {
		opts = append(opts, dialog.WithDiffMode(diffMode == "split"))
	}

	permDialog := dialog.NewPermissions(m.com, perm, opts...)
	m.dialog.OpenDialogWithGrace(permDialog)
	return nil
}

// openBatchFormDialog activates a tabbed multi-question form in
// the editor area. Single questions render without tabs or confirm.
func (m *UI) openBatchFormDialog(batch question.Request) {
	// Close any existing question form first to prevent stacking.
	if qf, ok := m.activeInline.(*dialog.QuestionForm); ok && qf != nil {
		m.activeInline = nil
	}

	form := dialog.NewQuestionForm(m.com.Styles, batch)
	form.OnAnswer = func(responses []question.Answer) {
		m.com.Workspace.QuestionAnswer(responses)
	}
	form.OnCancel = func() {
		m.com.Workspace.QuestionCancel()
	}
	m.activeInline = form
	m.editor.textarea.Blur()
	m.focus = uiFocusEditor
	m.activeInline.SetFocused(true)
	m.updateLayoutAndSize()
}

// handleQuestionNotification dismisses an open question form when
// any client resolved the pending batch. Only one question can be
// pending at a time, so any notification means the current form
// is stale regardless of BatchID.
func (m *UI) handleQuestionNotification(_ question.Notification) {
	if _, ok := m.activeInline.(*dialog.QuestionForm); ok {
		m.activeInline = nil
		m.editor.textarea.Focus()
		m.updateLayoutAndSize()
	}
}

// editorContentWidth returns the content width available to the
// editor area for the current state. It depends only on terminal
// width and layout (not on editor height), so it can be computed
// before the editor's height is known. This is the single source
// of truth for the inline editor width used by both layout sizing
// and Height() queries.
func (m *UI) editorContentWidth() int {
	width := m.width - 2 // appRect horizontal margins
	if m.state == uiChat && !m.isCompact {
		width -= 30 // sidebar column
	}
	return width
}

// completionsMaxWidth caps the "/"/"@" completions popup so it never
// outgrows the terminal: 60% of the terminal width, but no wider than the
// editor's own content width, since completionsPosition anchors the popup
// to the editor's cursor column.
func (m *UI) completionsMaxWidth() int {
	maxW := m.width * 6 / 10
	if ew := m.editorContentWidth(); ew > 0 && ew < maxW {
		maxW = ew
	}
	return maxW
}

// shouldCollapseQuestion reports whether a question form should render
// in its collapsed one-line view. This is true only when the form is
// unfocused and would consume more than half the terminal height.
func (m *UI) shouldCollapseQuestion(qf *dialog.QuestionForm) bool {
	return m.focus != uiFocusEditor && m.height > 0 && qf.Height(m.editorContentWidth()) > m.height*2/5
}

// handlePermissionNotification updates tool items when permission state changes.
func (m *UI) handlePermissionNotification(notification permission.PermissionNotification) {
	if toolItem := m.chat.MessageItem(notification.ToolCallID); toolItem != nil {
		if permItem, ok := toolItem.(chat.ToolMessageItem); ok {
			if notification.Granted {
				permItem.SetStatus(chat.ToolStatusRunning)
			} else {
				permItem.SetStatus(chat.ToolStatusAwaitingPermission)
			}
		}
	}

	// If this notification reflects a final resolution (granted or denied),
	// dismiss any open permissions dialog whose tool call ID matches. This
	// covers the case where another client resolved the request remotely.
	if !notification.Granted && !notification.Denied {
		return
	}
	if d := m.dialog.Dialog(dialog.PermissionsID); d != nil {
		if perm, ok := d.(*dialog.Permissions); ok && perm.ToolCallID() == notification.ToolCallID {
			m.dialog.CloseDialog(dialog.PermissionsID)
		}
	}
}

// handleAgentNotification translates domain agent events into desktop
// notifications using the UI notification backend.
func (m *UI) handleAgentNotification(n notify.Notification) tea.Cmd {
	var cmds []tea.Cmd
	switch n.Type {
	case notify.TypeAgentFinished:
		common.StopTurn()
		cmds = append(cmds, m.sendNotification(notification.Notification{
			Title:   notificationTitle(m.com.Workspace.WorkingDir()),
			Message: notificationBodyTaskFinished(n.SessionTitle),
		}))
	case notify.TypeAgentError:
		// Terminal edge like TypeAgentFinished, but the turn ended with an
		// error rather than a normal completion — surface it too instead of
		// leaving the user to notice the failure on their own.
		common.StopTurn()
		cmds = append(cmds, m.sendNotification(notification.Notification{
			Title:   notificationTitle(m.com.Workspace.WorkingDir()),
			Message: notificationBodyTaskFailed(n.Message),
		}))
	case notify.TypeReAuthenticate:
		return m.handleReAuthenticate(n.ProviderID)
	case notify.TypeAWSSSOAuth:
		return m.handleAWSSSOAuth(n.AWSSOCommand, n.AWSSOURL)
	case notify.TypeAWSSSOAuthResult:
		return m.handleAWSSSOAuthResult(n.Message)
	default:
		return nil
	}
	// TypeAgentFinished / TypeAgentError are the busy→idle edge: the agent
	// clears its active request before publishing precisely so observers
	// can re-probe. Drop the memoized busy state and re-fetch it and the
	// prompt queue off-thread.
	m.invalidateBusyCaches()
	m.invalidatePromptQueue()
	if cmd := m.dispatchBusyRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

func (m *UI) handleReAuthenticate(providerID string) tea.Cmd {
	cfg := m.com.Config()
	if cfg == nil {
		return nil
	}
	providerCfg, ok := cfg.Providers.Get(providerID)
	if !ok {
		return nil
	}
	if _, ok := cfg.Agents[config.AgentCoder]; !ok {
		return nil
	}
	// The coder agent leaves Model unset (it inherits the app's configured
	// model), so the model it actually runs on is always cfg.Model.
	return m.openAuthenticationDialog(providerCfg.ToProvider(), cfg.Model, config.SelectedModelTypeLarge)
}

// handleAWSSSOAuth opens the AWS SSO progress dialog (or updates the SSO URL
// on an already-open one). The refresh command runs in the coordinator; this
// dialog is a display surface driven by agent notifications.
func (m *UI) handleAWSSSOAuth(command, url string) tea.Cmd {
	// Update the URL on an already-open dialog.
	if existing := m.dialog.Dialog(dialog.AWSSSOID); existing != nil {
		if awsDlg, ok := existing.(*dialog.AWSSSO); ok && url != "" {
			awsDlg.SetURL(url)
		}
		m.dialog.BringToFront(dialog.AWSSSOID)
		return nil
	}
	if command == "" {
		return nil
	}
	dlg, cmd := dialog.NewAWSSSO(m.com, command)
	if url != "" {
		dlg.SetURL(url)
	}
	m.dialog.OpenDialogWithGrace(dlg)
	return cmd
}

// handleAWSSSOAuthResult finishes the AWS SSO dialog once the refresh command
// exits: it closes on success or shows the error so the user can dismiss it.
func (m *UI) handleAWSSSOAuthResult(errMsg string) tea.Cmd {
	existing := m.dialog.Dialog(dialog.AWSSSOID)
	if existing == nil {
		return nil
	}
	awsDlg, ok := existing.(*dialog.AWSSSO)
	if !ok {
		return nil
	}
	if errMsg == "" {
		// Success: the turn retries transparently, so no need to linger.
		m.dialog.CloseDialog(dialog.AWSSSOID)
		return nil
	}
	awsDlg.Finish(errMsg)
	return nil
}

// newSession clears the current session state and prepares for a new session.
// The actual session creation happens when the user sends their first message.
// Returns a command to reload prompt history.
func (m *UI) newSession() tea.Cmd {
	if !m.hasSession() {
		return nil
	}

	m.sessionLoadGen++
	m.sessionLoadExpectedID = ""
	m.session = nil
	m.sidebar.offset = 0
	m.sessionFiles = nil
	m.sessionFileReads = nil
	m.editor.pendingSendQueue = nil
	m.editor.pendingSendGen = 0
	m.editor.pendingSendLoading = false
	m.setState(uiLanding, uiFocusEditor)
	m.editor.textarea.Focus()
	m.chat.Blur()
	m.chat.ClearMessages()
	m.panel.expanded = false
	m.panel.autoExpanded = false
	m.panelTodosScrollOffset = 0
	m.wsCache.promptQueue = 0
	m.wsCache.promptQueueItems = nil
	m.wsCache.promptQueueCheckedAt = time.Now()
	m.invalidateBusyCaches()
	m.invalidatePromptQueue()
	m.editor.historyReset()
	agenttools.ResetCache()
	return tea.Batch(
		func() tea.Msg {
			m.com.Workspace.LSPStopAll(context.Background())
			return nil
		},
		m.loadPromptHistory(),
		m.reportCurrentSession(""),
	)
}

// checkBangModeAfterPaste engages bang mode when pasted text starts with
// optional whitespace followed by "!". It strips the prefix and adjusts
// the cursor, mirroring the keypress bang-mode entry logic.
func (m *UI) checkBangModeAfterPaste() {
	if m.editor.bangMode {
		return
	}
	val := m.editor.textarea.Value()
	trimmed := strings.TrimLeftFunc(val, unicode.IsSpace)
	if !strings.HasPrefix(trimmed, "!") {
		return
	}
	m.editor.bangMode = true
	m.editor.bangWasEmpty = true
	stripped := trimmed[1:]
	m.editor.textarea.SetValue(stripped)
	col := m.editor.textarea.Column()
	m.editor.textarea.SetCursorColumn(max(0, col-(len(val)-len(stripped))))
	m.setEditorPrompt(m.yoloModeCached())
}

// handlePasteMsg handles a paste message.
func (m *UI) handlePasteMsg(msg tea.PasteMsg) tea.Cmd {
	// Normalize \r\n before the textarea sanitizer sees it.
	msg.Content = strings.ReplaceAll(msg.Content, "\r\n", "\n")

	if m.dialog.HasDialogs() {
		return m.handleDialogMsg(msg)
	}

	if m.focus != uiFocusEditor {
		return nil
	}

	if hasPasteExceededThreshold(msg) {
		return func() tea.Msg {
			content := []byte(msg.Content)
			if int64(len(content)) > common.MaxAttachmentSize {
				return util.ReportWarn("Paste is too big (>5mb)")
			}
			name := fmt.Sprintf("paste_%d.txt", m.pasteIdx())
			return common.AttachmentFromBytes(name, name, content)
		}
	}

	// Attempt to parse pasted content as file paths. If possible to parse,
	// all files exist and are valid, add as attachments.
	// Otherwise, paste as text.
	paths := fsext.ParsePastedFiles(msg.Content)
	allExistsAndValid := func() bool {
		if len(paths) == 0 {
			return false
		}
		for _, path := range paths {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return false
			}

			lowerPath := strings.ToLower(path)
			isValid := false
			for _, ext := range common.AllowedImageTypes {
				if strings.HasSuffix(lowerPath, ext) {
					isValid = true
					break
				}
			}
			if !isValid {
				return false
			}
		}
		return true
	}
	if !allExistsAndValid() {
		prevHeight := m.editor.textarea.Height()
		cmd := m.updateTextareaWithPrevHeight(msg, prevHeight)
		m.checkBangModeAfterPaste()
		return cmd
	}

	var cmds []tea.Cmd
	for _, path := range paths {
		cmds = append(cmds, m.handleFilePathPaste(path))
	}
	return tea.Batch(cmds...)
}

func hasPasteExceededThreshold(msg tea.PasteMsg) bool {
	var (
		lineCount = 0
		colCount  = 0
	)
	for line := range strings.SplitSeq(msg.Content, "\n") {
		lineCount++
		colCount = max(colCount, len(line))

		if lineCount > pasteLinesThreshold || colCount > pasteColsThreshold {
			return true
		}
	}
	return false
}

// handleFilePathPaste handles a pasted file path.
func (m *UI) handleFilePathPaste(path string) tea.Cmd {
	return func() tea.Msg {
		fileInfo, err := os.Stat(path)
		if err != nil {
			return util.ReportError(err)
		}
		if fileInfo.IsDir() {
			return util.ReportWarn("Cannot attach a directory")
		}
		if fileInfo.Size() > common.MaxAttachmentSize {
			return util.ReportWarn("File is too big (>5mb)")
		}

		attachment, err := common.AttachmentFromPath(path)
		if err != nil {
			return util.ReportError(err)
		}

		return attachment
	}
}

// pasteImageFromClipboard reads image data from the system clipboard and
// creates an attachment. If no image data is found, it falls back to
// interpreting clipboard text as a file path.
func (m *UI) pasteImageFromClipboard() tea.Msg {
	imageData, err := clipboard.Read(clipboard.FormatImage)
	if int64(len(imageData)) > common.MaxAttachmentSize {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  "File too large, max 5MB",
		}
	}
	name := fmt.Sprintf("paste_%d.png", m.pasteIdx())
	if err == nil {
		return message.Attachment{
			FilePath: name,
			FileName: name,
			MimeType: mimeOf(imageData),
			Content:  imageData,
		}
	}

	textData, textErr := clipboard.Read(clipboard.FormatText)
	if textErr != nil || len(textData) == 0 {
		return nil // Clipboard is empty or does not contain an image
	}

	path := strings.TrimSpace(string(textData))
	path = strings.ReplaceAll(path, "\\ ", " ")
	if _, statErr := os.Stat(path); statErr != nil {
		return nil // Clipboard does not contain an image or valid file path
	}

	lowerPath := strings.ToLower(path)
	isAllowed := false
	for _, ext := range common.AllowedImageTypes {
		if strings.HasSuffix(lowerPath, ext) {
			isAllowed = true
			break
		}
	}
	if !isAllowed {
		return util.NewInfoMsg("File type is not a supported image format")
	}

	fileInfo, statErr := os.Stat(path)
	if statErr != nil {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  fmt.Sprintf("Unable to read file: %v", statErr),
		}
	}
	if fileInfo.Size() > common.MaxAttachmentSize {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  "File too large, max 5MB",
		}
	}

	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  fmt.Sprintf("Unable to read file: %v", readErr),
		}
	}

	return message.Attachment{
		FilePath: path,
		FileName: filepath.Base(path),
		MimeType: mimeOf(content),
		Content:  content,
	}
}

var pasteRE = regexp.MustCompile(`paste_(\d+).txt`)

func (m *UI) pasteIdx() int {
	result := 0
	for _, at := range m.editor.attachments.List() {
		found := pasteRE.FindStringSubmatch(at.FileName)
		if len(found) == 0 {
			continue
		}
		idx, err := strconv.Atoi(found[1])
		if err == nil {
			result = max(result, idx)
		}
	}
	return result + 1
}

// drawSessionDetails draws the session details in compact mode.
func (m *UI) drawSessionDetails(scr uv.Screen, area uv.Rectangle) {
	if m.session == nil {
		return
	}

	s := m.com.Styles

	width := area.Dx() - s.CompactDetails.View.GetHorizontalFrameSize()
	height := area.Dy() - s.CompactDetails.View.GetVerticalFrameSize()

	title := s.CompactDetails.Title.Width(width).MaxHeight(2).Render(m.session.Title)
	blocks := []string{
		title,
		"",
		m.modelInfo(width),
		"",
	}

	detailsHeader := lipgloss.JoinVertical(
		lipgloss.Left,
		blocks...,
	)

	version := s.CompactDetails.Version.Width(width).AlignHorizontal(lipgloss.Right).Render(version.Version)

	remainingHeight := height - lipgloss.Height(detailsHeader) - lipgloss.Height(version)

	const maxSectionWidth = 50
	sectionWidth := max(1, min(maxSectionWidth, width/4-2)) // account for spacing between sections
	maxItemsPerSection := remainingHeight - 3               // Account for section title and spacing

	lspSection := m.lspInfo(sectionWidth, maxItemsPerSection, false)
	mcpSection := m.mcpInfo(sectionWidth, maxItemsPerSection, false)
	skillsSection := m.skillsInfo(sectionWidth, maxItemsPerSection, false)
	filesSection := m.filesInfo(m.com.Workspace.WorkingDir(), sectionWidth, maxItemsPerSection, false)
	sections := lipgloss.JoinHorizontal(lipgloss.Top, filesSection, " ", lspSection, " ", mcpSection, " ", skillsSection)
	uv.NewStyledString(
		s.CompactDetails.View.
			Width(area.Dx()).
			Render(
				lipgloss.JoinVertical(
					lipgloss.Left,
					detailsHeader,
					sections,
					version,
				),
			),
	).Draw(scr, area)
}

func (m *UI) runMCPPrompt(clientID, promptID string, arguments map[string]string) tea.Cmd {
	load := func() tea.Msg {
		prompt, err := m.com.Workspace.GetMCPPrompt(clientID, promptID, arguments)
		if err != nil {
			// TODO: make this better
			return util.ReportError(err)()
		}

		if prompt == "" {
			return nil
		}
		return sendMessageMsg{
			Content: prompt,
		}
	}

	var cmds []tea.Cmd
	if cmd := m.dialog.StartLoading(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	cmds = append(cmds, load, func() tea.Msg {
		return closeDialogMsg{}
	})

	return tea.Sequence(cmds...)
}

func (m *UI) handleStateChanged() tea.Cmd {
	return m.updateAgentModelCmd(func() tea.Msg {
		if err := m.com.Workspace.UpdateAgentModel(context.Background()); err != nil {
			return util.NewErrorMsg(err)
		}
		return mcpStateChangedMsg{
			states: m.com.Workspace.MCPGetStates(),
		}
	})
}

func handleMCPPromptsEvent(ws workspace.MCPController, name string) tea.Cmd {
	return func() tea.Msg {
		ws.MCPRefreshPrompts(context.Background(), name)
		return nil
	}
}

func handleMCPToolsEvent(ws workspace.MCPController, name string) tea.Cmd {
	return func() tea.Msg {
		ws.RefreshMCPTools(context.Background(), name)
		return nil
	}
}

func handleMCPResourcesEvent(ws workspace.MCPController, name string) tea.Cmd {
	return func() tea.Msg {
		ws.MCPRefreshResources(context.Background(), name)
		return nil
	}
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

func (m *UI) enableDockerMCP() tea.Msg {
	ctx := context.Background()
	if err := m.com.Workspace.EnableDockerMCP(ctx); err != nil {
		return util.ReportError(err)()
	}

	return util.NewInfoMsg("Docker MCP enabled and started successfully")
}

func (m *UI) disableDockerMCP() tea.Msg {
	if err := m.com.Workspace.DisableDockerMCP(); err != nil {
		return util.ReportError(err)()
	}

	return util.NewInfoMsg("Docker MCP disabled successfully")
}

// renderLogo renders the Braid logo with the given styles and dimensions.
func renderLogo(t *styles.Styles, compact bool, width int) string {
	return logo.Render(t.Logo.GradCanvas, version.Version, compact, logo.Opts{
		FieldColor:   t.Logo.FieldColor,
		TitleColorA:  t.Logo.TitleColorA,
		TitleColorB:  t.Logo.TitleColorB,
		CharmColor:   t.Logo.CharmColor,
		VersionColor: t.Logo.VersionColor,
		Width:        width,
	})
}
