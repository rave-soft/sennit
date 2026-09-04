package model

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	fimage "github.com/rave-soft/sennit/internal/ui/image"
	"github.com/rave-soft/sennit/internal/ui/notification"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/threads"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

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

// copyChatHighlightMsg is sent to copy the current chat highlight to
// clipboard.
//
// uiOwned: dispatched from HandleMouseUp's double-click timer. Routed by
// active screen instead, releasing a drag-selection on one screen and
// switching screens before the timer fires copied the wrong UI's
// highlighted text (or none, if the other screen has no selection).
type copyChatHighlightMsg struct{ uiOwned }

// UI represents the main user interface model. Its fields fall into two
// kinds:
//
//   - Named sub-state structs (sess, queued, ops, lay, editor, lsp,
//     sidebar, panel, wsCache, threadList, threadsDock), each covering one
//     narrow concern (session lifecycle, layout, the editor, ...) and
//     referenced explicitly (m.sess.current, m.lay.width, ...). This is
//     the pre-existing convention in this package; each has its own doc
//     comment at its definition.
//   - Anonymously embedded groupings (widgets, term, notifyState,
//     breadcrumbState, integrationsState, mouseState) added for fields
//     that were flat on UI and didn't already have a narrow home. They are
//     embedded by value, not by pointer, for the same reason
//     appServices/appEvents/shutdownPhases are in internal/app/app.go: a
//     promoted field on a nil pointer embedding panics on first touch,
//     and this package's tests build bare UI{} values directly. Value
//     embedding keeps every field usable with its original m.field name
//     via Go's promotion rules; only struct literals that name these
//     fields (New below, and a few tests) need to qualify them by group.
//
// com, embedded, focus, state, and keyMap stay directly on UI: each is
// either the one shared dependency handle or reflects a piece of state
// central to the whole model rather than owned by one grouping.
type UI struct {
	com *common.Common

	sess sessionState

	// queued holds the chat placeholders for prompts submitted into a
	// busy session — see queuedPromptState.
	queued queuedPromptState

	// modelOperation owns correlation and in-flight state for model changes.
	modelOperation modelOperationState

	// permissionResponse owns the current permission request and response lifecycle.
	permissionResponse permissionResponseState

	// compactMode owns the persistence lifecycle for compact-mode changes.
	compactMode compactModeState

	// transparency owns the persistence lifecycle for transparency changes.
	transparency transparencyState

	// yolo owns the persistence lifecycle for yolo-mode changes.
	yolo yoloState

	// notificationStyle owns the persistence lifecycle for notification-style changes.
	notificationStyle notificationStyleState

	// themePreview owns the applied theme and preview restore point. UI keeps
	// validation, palette application, persistence, and command orchestration.
	themePreview themePreviewState

	// themePersistence owns correlation for theme-persistence results.
	themePersistence themePersistenceState

	lay layoutState

	// embedded is true for a UI instance attached to a thread's own
	// workspace rather than the top-level session — it skips
	// onboarding/initialize and doesn't drive the terminal progress bar,
	// since only one UI instance may own those.
	embedded bool

	focus uiFocusState
	state uiState

	keyMap KeyMap

	// goos is the platform configuredKeyMap renders bindings for
	// (super+ on darwin, ctrl+ elsewhere). Empty means "use runtime.GOOS",
	// which is always true outside tests — see withGOOS.
	goos string

	// cancellation owns the two-step agent cancellation confirmation.
	cancellation agentCancellationState

	// editor holds the prompt textarea, attachments, completions popup
	// state, bang (!) shell-mode flags, and prompt history. See editor.go.
	editor editorState

	// widgets holds the stateful sub-components (dialog stack, status
	// line, header, chat list, active inline editor). See widgets.go.
	widgets

	// term holds negotiated terminal capability/runtime state (capability
	// probe, keyboard enhancements, progress bar). See update_system.go.
	term

	// onboarding holds the project initialization prompt selection. See
	// initialize_selection_state.go.
	onboarding initializeSelectionState

	// lsp holds the memoized workspace LSP state and per-server diagnostic
	// counts. See lsp.go.
	lsp lspState

	// integrationsState holds MCP/skill/custom-command state loaded by
	// updateIntegrations. See update_integrations.go.
	integrationsState

	// sidebar holds virtual-scroll state and cached rendered content for the
	// chat sidebar. See sidebar.go.
	sidebar sidebarState

	// accountLabelsState caches each provider's active-account display
	// label for the sidebar's plan line, so rendering it never has to do
	// the file read ListAccounts implies. See account_label.go.
	accountLabelsState

	// notifyState holds desktop-notification backend/focus/per-thread
	// status state. See notifications.go.
	notifyState

	// breadcrumbState holds the breadcrumb bar's own state (crumbRoot,
	// hover, hit-test rect). See breadcrumbs.go.
	breadcrumbState

	// panel holds the expand state of the merged session panel (threads +
	// todos + queue, between chat and the editor). See session_panel.go.
	panel sessionPanelState

	// wsCache holds the memoized workspace busy/yolo/ready/model state and
	// its TTL-cache bookkeeping. See workspace_cache.go.
	wsCache workspaceCacheState

	// promptQueue owns the memoized prompt queue lifecycle. UI orchestrates
	// its workspace fetches, session scope, and layout effects.
	promptQueue promptQueueState

	// threadList holds the memoized thread list shared by the header
	// badge, the session panel's dock, and (via a pointer handed to
	// threads.New) the threads dashboard — one ListThreads round
	// trip serves all three. See threads_cache.go.
	threadList threads.ListCache

	// agentList holds the memoized delegation (task) list behind the
	// session panel's agents section. See agents_cache.go.
	agentList agentListCache

	// threadsDock holds the session panel's per-thread live activity
	// (in-progress todo, message count). See threads_dock.go /
	// session_panel.go.
	threadsDock threads.DockState

	// mouseState holds UI-level hover/click bookkeeping. See mouse.go.
	mouseState
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

// panelSurfacesThreads reports whether the session panel shows its threads
// block: surfacesThreads, and not while the user has drilled into a
// sub-agent's transcript.
//
// That transcript is somebody else's turn, read-only and already finished
// or running without you (see enterChildSession). The block is a way into
// the threads of the session you are driving, and there is no driving
// here -- offering it from a sub-agent's messages puts navigation into a
// view that has none.
//
// Deliberately narrower than surfacesThreads: the header badge and the
// refreshes behind it stay, because threads keep running while the
// transcript is being read and losing the only sign of that would hide
// live state rather than tidy it away.
func (m *UI) panelSurfacesThreads() bool {
	return m.surfacesThreads() && !m.sess.viewingChildSession()
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
	if sb := com.Config().Scrollbar(); sb != "" {
		scrollbarMode = sb
	}
	ch := chatlist.NewChat(com, scrollbarMode)

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

	header := newHeader(com)

	ui := &UI{
		com: com,
		editor: editorState{
			textarea:    ta,
			placeholder: newEditorPlaceholderState(),
			completions: completionsLifecycleState{popup: comp},
		},
		widgets: widgets{
			dialog: dialog.NewOverlay(),
			chat:   ch,
			header: header,
		},
		panel: sessionPanelState{
			spinner:       panelSpinner,
			hoveredThread: -1,
			hoveredAgent:  -1,
		},
		lsp: lspState{
			states: make(map[string]workspace.LSPClientInfo),
		},
		integrationsState: integrationsState{
			mcpStates:   make(map[string]workspace.MCPClientInfo),
			skillStates: com.Workspace.SkillStates(),
		},
		notifyState: notifyState{
			notifyBackend:       notification.NoopBackend{},
			notifyWindowFocused: true,
		},
		sess: sessionState{
			initialSessionID:    initialSessionID,
			continueLastSession: continueLast,
		},
	}
	for _, opt := range opts {
		opt(ui)
	}
	// withGOOS may have set ui.goos above; resolve to the host platform
	// otherwise. keyMap/attachments are built here, after opts, so a test
	// pinning the platform actually controls what gets rendered.
	if ui.goos == "" {
		ui.goos = runtime.GOOS
	}
	ui.keyMap = configuredKeyMap(ui.goos, com.Config().Keybindings())
	ui.editor.attachments = attachments.New(
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
			DeleteMode: ui.keyMap.Editor.AttachmentDeleteMode,
			DeleteAll:  ui.keyMap.Editor.DeleteAllAttachments,
			Escape:     ui.keyMap.Editor.Escape,
		},
	)

	status := NewStatus(com, ui)

	// Seed the yolo cache once at construction; afterwards it is kept
	// fresh by write-through toggles and off-thread refreshes so Update
	// and View never probe the workspace synchronously.
	yolo := com.Workspace.PermissionSkipRequests()
	ui.wsCache.yoloCache.Set(yolo)

	// Seed the memoized agent ready/model state the same way so the first
	// frame renders the model info; the busy probe keeps it fresh
	// afterwards.
	if com.Workspace.AgentIsReady() {
		ui.wsCache.agentCache.Set(agentReadyModel{ready: true, model: com.Workspace.AgentModel()})
	}
	ui.setEditorPrompt(yolo)
	ui.editor.textarea.Placeholder = ui.editor.placeholder.ready
	ui.status = status

	// Initialize compact mode from config
	ui.lay.forceCompactMode = com.Config().CompactMode()

	desiredState := uiLanding
	desiredFocus := uiFocusEditor
	if ui.embedded {
		// A thread's embedded chat never onboards or initializes — those
		// are one-time, top-level-session concerns.
		//
		// It opens straight into the chat frame when it already knows
		// which session it is for, rather than painting the landing screen
		// for the moment the load takes. The router switches to this UI
		// as soon as the attach returns (see Root.handleThreadAttached),
		// which is well before the session's messages are read, so landing
		// first meant clicking a thread flashed a full logo-and-status
		// screen on the way in. Empty is the right frame to wait in: it is
		// the one the content lands in, and every part of it tolerates
		// having no session yet.
		if initialSessionID != "" {
			desiredState = uiChat
		}
	} else if !com.Config().IsConfigured() {
		desiredState = uiOnboarding
	} else if n, _ := com.Workspace.ProjectNeedsInitialization(); n {
		desiredState = uiInitialize
	}

	// set initial state
	ui.setState(desiredState, desiredFocus)

	cfg := com.Config()

	// disable indeterminate progress bar
	ui.progressBarEnabled = cfg.Options == nil || cfg.Options.Progress == nil || *cfg.Options.Progress
	// enable transparent mode
	ui.lay.isTransparent = cfg.TransparentEnabled()
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
	cmds = append(cmds, loadCustomCommands(m.com, m))
	// load prompt history async
	cmds = append(cmds, m.sess.loadPromptHistory(m.com, m))
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
	cmds = append(cmds, checkPendingMCPAuth(m.com, m))
	// Prime the sidebar's account-label cache for whatever model/provider
	// this UI already knows about at construction time (see
	// account_label.go). refreshAccountLabelCmd is nil for a nil model,
	// so this is a no-op during onboarding.
	if model := m.viewedModel(); model != nil {
		if cmd := refreshAccountLabelCmd(m.com, m, model.ModelCfg.Provider); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
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
	n := len(m.com.Workspace.ConfigProblems())
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
	case m.state == uiOnboarding || m.state == uiInitialize:
		// Nothing to load until the workspace is set up: those two states
		// own the screen and end by moving to one of the states below.
		//
		// Both onboarding and initialize must be excluded here, not just
		// uiLanding: a thread's embedded chat can open straight into
		// uiChat when it already knows its session (see New), and a
		// narrower check would refuse the load it was opening for,
		// leaving the frame it opened into blank forever.
		return nil
	case m.sess.initialSessionID != "":
		return m.requestSessionLoad(m.sess.initialSessionID)
	case m.sess.continueLastSession:
		ws := m.com.Workspace
		ctx := m.com.Context()
		return func() tea.Msg {
			sessions, err := ws.ListSessions(ctx)
			if err != nil || len(sessions) == 0 {
				return nil
			}
			return requestSessionLoad{uiOwned: uiOwned{owner: m}, sessionID: sessions[0].ID}
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
//
// Discovery — walking config directories, merging in the skill catalog —
// is the workspace's job, not the palette's: this just asks for the list
// and renders it. Whatever could be read is still returned when part of
// it failed, so a broken commands directory costs the skills nothing.
func loadCustomCommands(com *common.Common, owner *UI) tea.Cmd {
	ws := com.Workspace
	ctx := com.Context()
	return func() tea.Msg {
		customCommands, err := ws.ListCustomCommands(ctx)
		if err != nil {
			slog.Error("Failed to load custom commands", "error", err)
		}
		return userCommandsLoadedMsg{uiOwned: uiOwned{owner: owner}, Commands: customCommands}
	}
}

// updateGroupFn is the contract every update* handler dispatched through
// updateGroups already has: try to consume msg, returning the possibly
// grown cmd slice and whether it was handled. done ends Update immediately
// with cmds batched, exactly like the switch cases this table replaces.
type updateGroupFn func(m *UI, msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool)

// updateGroups maps a message's concrete type to the update* handler that
// owns it. It used to be nine separate multi-type case clauses in Update's
// switch — wiring a new message type into one of these handlers meant
// finding its case list in that switch and adding to it. Now it's one
// reflect.TypeFor line in buildUpdateGroups. Lookup is by exact
// reflect.Type rather than sequential case matching, so — unlike a type
// switch — entry order can never matter; that's safe here because every
// type below belongs to exactly one handler (verified: none of these types
// appears in more than one of buildUpdateGroups' groups, nor in Update's
// remaining bespoke cases).
var updateGroups = buildUpdateGroups()

// buildUpdateGroups constructs updateGroups once at package init. Each
// group lists exactly the message types routed to that handler, rather
// than matched in Update's remaining bespoke switch.
func buildUpdateGroups() map[reflect.Type]updateGroupFn {
	g := make(map[reflect.Type]updateGroupFn, 64)
	register := func(fn updateGroupFn, types ...reflect.Type) {
		for _, t := range types {
			g[t] = fn
		}
	}

	register((*UI).updateSystem,
		reflect.TypeFor[tea.EnvMsg](), reflect.TypeFor[tea.ModeReportMsg](),
		reflect.TypeFor[uv.UnknownOscEvent](), reflect.TypeFor[tea.FocusMsg](),
		reflect.TypeFor[tea.BlurMsg](), reflect.TypeFor[tea.WindowSizeMsg](),
		reflect.TypeFor[tea.KeyboardEnhancementsMsg](), reflect.TypeFor[spin.StepMsg](),
		reflect.TypeFor[chatlist.ScrollbarHideMsg](), reflect.TypeFor[chatlist.WarmMsg](),
		reflect.TypeFor[sidebarScrollbarHideMsg](), reflect.TypeFor[spinner.TickMsg](),
		reflect.TypeFor[uv.KittyGraphicsEvent]())

	register((*UI).updateSession,
		reflect.TypeFor[sessionsLoadedMsg](), reflect.TypeFor[busyStateMsg](),
		reflect.TypeFor[promptQueueMsg](), reflect.TypeFor[agentRunSubmittedMsg](),
		reflect.TypeFor[loadSessionMsg](), reflect.TypeFor[requestSessionLoad](),
		reflect.TypeFor[sessionFilesUpdatesMsg](), reflect.TypeFor[sendMessageMsg](),
		reflect.TypeFor[pubsub.Event[session.Session]](), reflect.TypeFor[pubsub.Event[message.Message]](),
		reflect.TypeFor[pubsub.Event[history.File]](), reflect.TypeFor[sendMessageErrorMsg](),
		reflect.TypeFor[sendPendingQueueMsg](), reflect.TypeFor[bangSessionCreatedMsg](),
		reflect.TypeFor[createSessionMsg]())

	register((*UI).updateIntegrations,
		reflect.TypeFor[lspStatesMsg](), reflect.TypeFor[userCommandsLoadedMsg](),
		reflect.TypeFor[mcpStateChangedMsg](), reflect.TypeFor[mcpPromptsLoadedMsg](),
		reflect.TypeFor[promptHistoryLoadedMsg](), reflect.TypeFor[pubsub.Event[workspace.LSPEvent]](),
		reflect.TypeFor[pubsub.Event[skills.Event]](), reflect.TypeFor[dialog.ActionMCPAuthStarted](),
		reflect.TypeFor[dialog.ActionMCPAuthComplete](), reflect.TypeFor[dialog.ActionMCPAuthErrored](),
		reflect.TypeFor[pubsub.Event[workspace.MCPEvent]](), reflect.TypeFor[accountLabelsLoadedMsg]())

	register((*UI).updatePrompts,
		reflect.TypeFor[closeDialogMsg](), reflect.TypeFor[pubsub.Event[permission.PermissionRequest]](),
		reflect.TypeFor[pubsub.Event[permission.PermissionNotification]](), reflect.TypeFor[pubsub.Event[question.Request]](),
		reflect.TypeFor[pubsub.Event[question.Notification]]())

	register((*UI).updateSettings,
		reflect.TypeFor[providerConfiguredResult](), reflect.TypeFor[modelSelectResult](),
		reflect.TypeFor[agentModelInitializedMsg](), reflect.TypeFor[modelSettingUpdatedMsg](),
		reflect.TypeFor[transparentToggledMsg](), reflect.TypeFor[themeSetMsg](),
		reflect.TypeFor[compactModeToggledMsg](), reflect.TypeFor[notificationStyleSetMsg](),
		reflect.TypeFor[permissionResponseMsg](), reflect.TypeFor[yoloToggledMsg](),
		reflect.TypeFor[notificationSentMsg](), reflect.TypeFor[importCopilotResult]())

	register((*UI).updateMouse,
		reflect.TypeFor[chatlist.DelayedClickMsg](), reflect.TypeFor[tea.MouseClickMsg](),
		reflect.TypeFor[tea.MouseMotionMsg](), reflect.TypeFor[tea.MouseReleaseMsg](),
		reflect.TypeFor[common.CoalescedWheelMsg]())

	register((*UI).updateShell,
		reflect.TypeFor[openEditorMsg](), reflect.TypeFor[shellStreamMsg](),
		reflect.TypeFor[shellResultMsg]())

	register((*UI).updateStatus,
		reflect.TypeFor[util.InfoMsg](), reflect.TypeFor[util.ClearStatusMsg](),
		reflect.TypeFor[pubsub.Event[workspace.AgentNotification]](), reflect.TypeFor[cancelTimerExpiredMsg]())

	register((*UI).updateThreads,
		reflect.TypeFor[pubsub.Event[proto.Thread]](), reflect.TypeFor[threads.LoadedMsg](),
		reflect.TypeFor[threads.DockActivityLoadedMsg](), reflect.TypeFor[agentsLoadedMsg]())

	return g
}

// Update handles updates to the UI model.
func (m *UI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	// Update terminal capabilities
	m.caps.Update(msg)
	m.freezeFinishedChildDelegation()
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
	if fn, ok := updateGroups[reflect.TypeOf(msg)]; ok {
		var done bool
		if cmds, done = fn(m, msg, cmds); done {
			return m, tea.Batch(cmds...)
		}
	} else {
		switch msg := msg.(type) {
		case agentModelChangedMsg:
			// The coordinator model changed (selection, thinking, reasoning):
			// re-fetch the memoized ready/model state off-thread.
			m.wsCache.invalidateBusyCaches()
			if cmd := m.dispatchBusyRefresh(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		case tea.TerminalVersionMsg:
			return m.updateTerminalVersion(msg)
		case copyChatHighlightMsg:
			cmds = append(cmds, m.copyChatHighlight(m))
		case clearChatMouseMsg:
			m.chat.ClearMouse()
		case fileCompletionMsg:
			m.sess.fileReads = append(m.sess.fileReads, msg.absPath)
			_ = m.editor.attachments.Update(msg.attachment)
		case pasteFilesCheckedMsg:
			if cmd := m.applyPasteFilesChecked(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		case openEditorReadyMsg:
			cmds = append(cmds, execEditorCmd(msg, m))
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
		case richPasteMsg:
			if cmd := m.handleRichPaste(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		case completions.CompletionItemsLoadedMsg:
			if m.editor.completions.open {
				m.editor.completions.setItems(msg.Files, msg.Resources)
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
	}

	if m.focus == uiFocusEditor {
		m.editor.textarea.Placeholder = m.editor.placeholder.selectPlaceholder(editorPlaceholderContext{
			current:                  m.editor.textarea.Placeholder,
			editorFocused:            true,
			viewingChildSession:      m.sess.viewingChildSession(),
			exitChildSessionShortcut: m.exitChildSessionShortcut(),
			bangActive:               m.editor.bang.isActive(),
			busy:                     m.isAgentBusy(),
			yolo:                     m.wsCache.yoloModeCached(),
		})
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
// session.CreateAgentToolSessionID(messageID, toolCallID), together with
// the display data captured from its chat item at the moment the sibling
// list was built (see Chat.NestedToolContainerRefs). Carrying that
// snapshot is what lets alt+left/alt+right cycle between siblings without
// the parent's chat items, which are no longer loaded once navigation has
// descended into a child session.
type childSessionRef struct {
	messageID  string
	toolCallID string

	// label, agentName, model, effort, delegationStart and
	// delegationDuration mirror the same-named sessionNavFrame fields; see
	// their doc there. All zero for a ref built from an item that isn't a
	// resolvable delegation.
	label                    string
	agentName, model, effort string
	delegationStart          time.Time
	delegationDuration       time.Duration
}

// sessionNavFrame is one level of the sub-agent session-navigation stack
// (m.sess.navStack): where alt+up should return to, and the sibling
// delegations in that parent chat that alt+left/alt+right cycle through.
type sessionNavFrame struct {
	parentSessionID string
	// parentTitle is the parent session's title, captured at
	// enterChildSession time. m.sess.current is repointed to the child as soon
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

	// childSessionID is the session this frame descends into — the id
	// requestSessionLoad was given. Kept so the delegation's own busy
	// state can be asked about by id (see childDelegationBusy) rather
	// than inferred from whichever session happens to be loaded.
	childSessionID string
	// delegationSawBusy records that this delegation was observed
	// generating at least once while being viewed, which is what lets
	// freezeFinishedChildDelegation tell "finished just now, freeze the
	// elapsed time" apart from "was already finished before we got
	// here, its runtime is unknown".
	delegationSawBusy bool
}

// adoptRef copies a sibling's captured delegation data onto the frame,
// which is everything the child-session panel needs to describe the level
// the frame points at. Used both when the frame is pushed and when
// alt+left/alt+right moves it to another sibling.
func (f *sessionNavFrame) adoptRef(ref childSessionRef) {
	f.label = ref.label
	if f.label == "" {
		f.label = defaultChildSessionLabel
	}
	f.agentName, f.model, f.effort = ref.agentName, ref.model, ref.effort
	f.delegationStart, f.delegationDuration = ref.delegationStart, ref.delegationDuration
	f.delegationSawBusy = false
}

// childDelegationBusy reports whether the delegation the frame points at
// is generating right now. Deliberately by session id rather than via
// [UI.isCurrentSessionBusy]: navigation is asynchronous, so the loaded
// session briefly remains the parent after a frame is pushed, and the
// parent is busy for as long as its delegation runs.
func childDelegationBusy(com *common.Common, frame sessionNavFrame) bool {
	if frame.childSessionID == "" || com == nil || com.Workspace == nil {
		return false
	}
	return com.Workspace.AgentIsSessionBusy(frame.childSessionID)
}

// freezeFinishedChildDelegation stops the viewed delegation's elapsed
// time at the moment its child session finishes, instead of letting the
// reading vanish along with the busy state it is gated on.
//
// The end timestamp isn't available here — it lands in the parent
// session, whose chat isn't loaded while a child is being viewed — so
// this freezes on the transition, exactly as SetResult does for a live
// delegation block. Runs from Update rather than from the draw path both
// because the view must not mutate state and because the transition has
// to be caught when it happens: a finished delegation produces no further
// frames to notice it in.
func (m *UI) freezeFinishedChildDelegation() {
	if len(m.sess.navStack) == 0 {
		return
	}
	frame := &m.sess.navStack[len(m.sess.navStack)-1]
	if frame.delegationDuration > 0 || frame.delegationStart.IsZero() {
		return
	}
	if childDelegationBusy(m.com, *frame) {
		frame.delegationSawBusy = true
		return
	}
	if !frame.delegationSawBusy {
		// Already finished when we arrived: its runtime is whatever the
		// captured ref said, and "unknown" is the honest answer if that
		// was nothing. Timing it from here would report how long the
		// session has been open.
		return
	}
	frame.delegationSawBusy = false
	frame.delegationDuration = time.Since(frame.delegationStart)
}

// handleSelectModel performs the model selection after any provider
// pre-checks have completed.  The ImportCopilot, UpdatePreferredModel, and
// initAgentAndReportModel steps run sequentially via typed result messages;
// errors stop the chain without a false success.
func (m *UI) handleSelectModel(msg dialog.ActionSelectModel) tea.Cmd {
	var cmds []tea.Cmd

	if m.modelOperation.isLoading() {
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
		generation, started := m.modelOperation.begin()
		if !started {
			return util.ReportWarn("Model settings are already being updated")
		}
		ws := m.com.Workspace
		cmds = append(cmds, func() tea.Msg {
			ws.ImportCopilot()
			return importCopilotResult{
				uiOwned:      uiOwned{owner: m},
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
	generation, started := m.modelOperation.begin()
	if !started {
		return util.ReportWarn("Model settings are already being updated")
	}
	capturedModel := msg.Model
	ws := m.com.Workspace
	cmds = append(cmds, func() tea.Msg {
		if err := ws.UpdatePreferredModel(config.ScopeGlobal, capturedModel); err != nil {
			return modelSelectResult{uiOwned: uiOwned{owner: m}, Err: err, generation: generation}
		}
		return modelSelectResult{uiOwned: uiOwned{owner: m}, Onboarding: isOnboarding, Model: capturedModel, generation: generation}
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
	return updateAgentModelCmd(m, func() tea.Msg {
		// InitCoderAgent brings the coder agent up for the first time
		// (onboarding); it must complete before UpdateAgentModel touches
		// it, so both run in this single off-thread step rather than as
		// separate commands racing each other.
		if isOnboarding {
			if err := ws.InitCoderAgent(ctx); err != nil {
				return agentModelInitializedMsg{uiOwned: uiOwned{owner: m}, Err: err, Onboarding: isOnboarding, Model: model, generation: generation}
			}
		}
		if err := ws.UpdateAgentModel(ctx); err != nil {
			return agentModelInitializedMsg{uiOwned: uiOwned{owner: m}, Err: err, Onboarding: isOnboarding, Model: model, generation: generation}
		}
		return agentModelInitializedMsg{uiOwned: uiOwned{owner: m}, Onboarding: isOnboarding, Model: model, generation: generation}
	})
}

// activeThreadBadgeCount is the header badge's active-thread count, or 0
// where threads are not this UI's business (see surfacesThreads).
func (m *UI) activeThreadBadgeCount() int {
	if !m.surfacesThreads() {
		return 0
	}
	return threads.ActiveCount(m.threadList.Threads())
}

func currentModelSupportsImages(com *common.Common) bool {
	cfg := com.Config()
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
	generation, started := m.compactMode.begin()
	if !started {
		return util.ReportWarn("Compact mode is already being updated")
	}
	desired := !m.lay.forceCompactMode
	workspace := m.com.Workspace
	return func() tea.Msg {
		return compactModeToggledMsg{uiOwned: uiOwned{owner: m}, Err: workspace.SetCompactMode(config.ScopeGlobal, desired), Enabled: desired, generation: generation}
	}
}

// isAgentBusy returns true if the agent coordinator exists and is currently
// busy processing a request. It only reads the memoized state (it runs in
// per-message paths like the textarea placeholder, where a workspace probe
// is treated as IO); the value is refreshed off-thread, see
// workspace_cache.go.
func (m *UI) isAgentBusy() bool {
	if m.editor.bang.isRunning() {
		return true
	}
	return m.wsCache.agentBusyCache.Value
}

// isCurrentSessionBusy reports whether the agent is generating for the session
// the chat currently has loaded. Deliberately not [UI.isAgentBusy], which
// answers the workspace-wide question and is true whenever any other session,
// thread, or background task is running — see applySessionMessageItems for why
// the difference matters. Unlike that one this is a direct probe, not the
// memoized value: it is a lookup in the dispatcher's active-request map, and
// it runs on session load rather than per message.
func (m *UI) isCurrentSessionBusy() bool {
	if !m.sess.hasSession() || m.com == nil || m.com.Workspace == nil {
		return false
	}
	return m.com.Workspace.AgentIsSessionBusy(m.sess.current.ID)
}

// hasSession returns true if there is an active session with a valid ID.
func (s *sessionState) hasSession() bool {
	return s.current != nil && s.current.ID != ""
}

// applyTheme switches the live palette and persists the choice. The swap is
// synchronous so the next frame is already in the new colors; the write to
// disk happens off the Update goroutine and reverts the swap if it fails,
// rather than leaving the UI in a palette the config does not agree with.
func (m *UI) applyTheme(id string) tea.Cmd {
	if !styles.IsKnownPaletteID(id) {
		m.cancelThemePreview()
		return util.ReportError(fmt.Errorf("unknown theme %q", id))
	}
	// A preview that ends in a choice is kept, so the palette to fall back
	// to on a failed write is the configured one, not whatever the cursor
	// happened to be resting on.
	previous := styles.PaletteByID(m.com.Config().ThemeID()).ID
	m.themePreview.confirm()
	if id == previous && m.liveThemeID() == previous {
		return nil
	}

	cmd := m.setTheme(id)
	generation := m.themePersistence.begin()
	ws := m.com.Workspace
	return tea.Batch(cmd, func() tea.Msg {
		return themeSetMsg{
			uiOwned:    uiOwned{owner: m},
			Err:        ws.SetConfigField(config.ScopeGlobal, "options.tui.theme", id),
			ID:         id,
			Previous:   previous,
			generation: generation,
		}
	})
}

// liveThemeID returns the palette currently drawn, which is the previewed
// one while the theme picker is browsing and the configured one otherwise.
func (m *UI) liveThemeID() string {
	configured := styles.PaletteByID(m.com.Config().ThemeID()).ID
	return m.themePreview.live(configured)
}

// previewTheme paints the whole UI in the palette the theme picker is
// resting on, without touching config. Nothing is persisted until the user
// confirms; [UI.cancelThemePreview] puts the previous palette back if they
// walk away instead.
func (m *UI) previewTheme(id string) tea.Cmd {
	if !styles.IsKnownPaletteID(id) {
		return nil
	}
	configured := styles.PaletteByID(m.com.Config().ThemeID()).ID
	if !m.themePreview.preview(id, configured) {
		return nil
	}
	return m.setTheme(id)
}

// cancelThemePreview restores the palette that was live before the theme
// picker started previewing. It is a no-op when no preview is in progress.
func (m *UI) cancelThemePreview() tea.Cmd {
	if !m.themePreview.previewActive() {
		return nil
	}
	from, needed := m.themePreview.cancel(m.liveThemeID())
	if !needed {
		return nil
	}
	return m.setTheme(from)
}

// setTheme replaces the styles every component shares and drops the render
// caches that hold strings colored by the old palette. Most of the UI reads
// com.Styles at draw time and needs nothing; the rest — cached renders and
// the widgets that copied a style at construction — is refreshed here.
// Anything added to this list must also be reachable from a widget that
// outlives a theme switch; short-lived views rebuild themselves anyway.
//
// The returned command re-arms the animations that had to be rebuilt (see
// [Chat.Restyle]); it is nil when nothing was animating.
func (m *UI) setTheme(id string) tea.Cmd {
	// WithSpinner, because this is a wholesale replacement: the motion
	// setting is config-derived and a palette cannot carry it, so
	// without re-applying it here the first /theme switch of a session
	// silently put every spinner back to the scramble.
	//
	// A new pointer, not an in-place `*m.com.Styles = ...`: com is on the
	// uicheck opt-out list a tea.Cmd closure may capture, on the theory
	// that com is set once and never reassigned — true of the com field
	// itself, but overwriting *m.com.Styles in place broke it one level
	// down. beginSessionLoad snapshots `styles := m.com.Styles` and hands
	// that pointer into a command built off the Update goroutine; an
	// in-place write here raced that command's reads of the very struct
	// it was promised was a stable snapshot. Swapping the pointer instead
	// leaves every already-issued snapshot exactly as it was — a frozen
	// copy of the old palette — while new readers of m.com.Styles see the
	// new one.
	newStyles := styles.Theme(id).WithSpinner(common.SpinnerMode(m.com.Workspace))
	m.com.Styles = &newStyles
	m.themePreview.setLive(styles.PaletteByID(id).ID)
	t := m.com.Styles

	var cmd tea.Cmd
	if m.status != nil {
		m.status.Restyle()
	}
	if m.header != nil {
		// The wordmark's gradient is rendered once and cached.
		m.header.refresh()
	}
	if m.chat != nil {
		cmd = m.chat.Restyle()
	}

	// Editor widgets copy their styles at construction and live for the
	// whole session, so they keep the old palette unless told otherwise.
	m.editor.textarea.SetStyles(t.Editor.Textarea)
	if m.editor.completions.popup != nil {
		m.editor.completions.popup.SetStyles(completions.PopupStyles{
			Normal:         t.Completions.Normal,
			Focused:        t.Completions.Focused,
			Match:          t.Completions.Match,
			Muted:          t.Completions.Muted,
			Border:         t.Completions.Border,
			ScrollbarThumb: t.Dialog.ScrollbarThumb,
			ScrollbarTrack: t.Dialog.ScrollbarTrack,
		})
	}
	if m.editor.attachments != nil {
		m.editor.attachments.Renderer().SetStyles(
			t.Attachments.Normal,
			t.Attachments.Deleting,
			t.Attachments.Image,
			t.Attachments.Text,
			t.Attachments.Skill,
			t.Attachments.Remove,
			t.Attachments.RemoveHover,
		)
	}
	m.panel.spinner.Style = t.Pills.TodoSpinner

	// Dialogs are usually rebuilt on open, but the theme picker (and the
	// commands palette behind it) are on screen while the palette changes.
	if m.dialog != nil {
		m.dialog.Restyle()
	}

	m.sidebar.cacheSidebarLogo(m.com, m.lay.layout.sidebar.Dx())
	m.updateLayoutAndSize()
	return cmd
}

// attachSkill reads a skill's content by ID and returns it as a markdown
// attachment to be added to the attachment toolbar. The user can then
// compose a message and send it with the skill attached.
// The name parameter is used as a fallback when the server does not
// return one.
func attachSkill(com *common.Common, skillID, name string) tea.Cmd {
	ws := com.Workspace
	ctx := com.Context()
	return func() tea.Msg {
		content, result, err := ws.ReadSkill(ctx, skillID)
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

// uiOwned: dispatched by handleSelectModel's Copilot-import branch. Routed
// by active screen instead, importing Copilot tokens from a thread's own
// models dialog could apply to the main screen's modelOperation state
// instead, leaving whichever one actually started it stuck "already being
// updated".
type importCopilotResult struct {
	uiOwned

	providerID   string
	model        config.SelectedModel
	isOnboarding bool
	generation   uint64
}

// sendMessageErrorMsg carries an error from a sendMessage cmd. The Update
// handler converts it into a util.InfoMsg and clears the optimistic busy
// state (already done inside the cmd).
//
// uiOwned: dispatched by sendMessageNow and runShellCommandInternal's
// session-create path. Routed by active screen instead, an error that
// lands while the dashboard is up never clears pendingSendActive (or,
// for the creating path, pendingSendLoading), so every later send just
// queues behind a turn that already failed.
type sendMessageErrorMsg struct {
	uiOwned

	Err            error
	generation     uint64
	sessionID      string
	loadGeneration uint64
	creating       bool
	// content is the prompt text this send attempted, so a failure can
	// drop the "queued" placeholder it drew before dispatch — see
	// applySendMessageError. Empty on the creating path, which never
	// shows one (queued.show only runs once m.sess.current is set).
	content string
}

// bangSessionCreatedMsg is returned by runShellCommandInternal when a bang
// command triggered a session creation; the Update handler uses it to load
// the session and then starts the shell command.
//
// uiOwned for the same reason as sendMessageErrorMsg: dropped on the
// dashboard, the shell command it was meant to start never runs and
// pendingSendLoading never clears.
type bangSessionCreatedMsg struct {
	uiOwned

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
	uiOwned

	gen               uint64
	sessions          []session.Session
	selectedSessionID string
	err               error
}

// startNewSessionGuarded appends m.newSession()'s command unless the agent
// is busy, in which case it appends a warning instead. started reports
// which happened, for callers (handleMainKeyPress) that need to do
// something else — moving focus — only on the path that actually started a
// session. Shared by the three places "start a new session" can be
// triggered (the Chat.NewSession key binding from both focus states, and
// the commands palette's ActionNewSession) so the busy guard and its
// wording can't drift between them.
func (m *UI) startNewSessionGuarded(cmds []tea.Cmd) (out []tea.Cmd, started bool) {
	if m.isAgentBusy() {
		return append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session...")), false
	}
	if cmd := m.newSession(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return cmds, true
}

// newSession clears the current session state and prepares for a new session.
// The actual session creation happens when the user sends their first message.
// Returns a command to reload prompt history.
func (m *UI) newSession() tea.Cmd {
	if !m.sess.hasSession() {
		return nil
	}

	m.sess.loadGen++
	m.sess.loadExpectedID = ""
	m.sess.current = nil
	m.sess.descendantCost = 0
	m.clearChildSessionNav()
	m.sidebar.offset = 0
	m.sess.files = nil
	m.sess.filesVersion++
	m.sess.fileReads = nil
	m.editor.pendingSend.discardForNewSession()
	m.setState(uiLanding, uiFocusEditor)
	m.editor.textarea.Focus()
	m.chat.Blur()
	m.chat.ClearMessages()
	m.panel.expanded = false
	m.panel.autoExpanded = false
	m.panel.todosScrollOffset = 0
	m.wsCache.invalidateBusyCaches()
	m.promptQueue.clear()
	m.editor.historyReset()
	ws := m.com.Workspace
	ctx := m.com.Context()
	ws.ResetAgentToolCache()
	return tea.Batch(
		func() tea.Msg {
			ws.LSPStopAll(ctx)
			return nil
		},
		m.sess.loadPromptHistory(m.com, m),
		m.sess.reportCurrentSession(m.com, ""),
	)
}

// clearChatMouseMsg asks Update to clear the chat's mouse selection state
// after a copy completes. copyChatHighlight's callback runs on the tea.Cmd
// goroutine, so it must not touch m.chat directly.
//
// uiOwned: dispatched by copyChatHighlight's clipboard-copy callback.
// Routed by active screen instead, this cleared whichever UI's chat
// happened to be on top instead of the one whose highlight was actually
// copied, leaving that UI's selection stuck visible.
type clearChatMouseMsg struct{ uiOwned }

func (w *widgets) copyChatHighlight(owner *UI) tea.Cmd {
	text := w.chat.HighlightContent()
	return common.CopyToClipboardWithCallback(
		text,
		"Selected text copied to clipboard",
		func() tea.Msg {
			return clearChatMouseMsg{uiOwned{owner: owner}}
		},
	)
}
