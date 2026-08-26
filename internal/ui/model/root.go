package model

// Root is the top-level Bubble Tea model (see internal/cmd/root.go). It
// routes between three screens — the main session UI, the threads
// dashboard, and an attached thread's own embedded UI — and owns the pieces
// that don't belong to any single screen: the thread cache/dashboard is
// lazily created on first use, and an attached thread's workspace/event
// pump lives here rather than inside *UI, since only one *UI (main) is ever
// the terminal's progress-bar owner.
//
// Screens are mutually exclusive and each one's Update/View is delegated to
// wholesale — Root itself never draws chat or list rows, only chooses which
// child does. This mirrors the "UI is the sole Bubble Tea model" rule in
// internal/ui/AGENTS.md one level up: Root is now that sole model, and each
// screen is a stateful, imperatively-driven child exactly like Chat/List are
// to UI.

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// showThreadsDashboardMsg requests switching to the threads dashboard
// screen. Handled by the Root router below; a bare *UI has no dashboard
// screen of its own, so this falls through Update's default case
// harmlessly when UI is driven directly (e.g. in tests).
type showThreadsDashboardMsg struct{}

// screenID identifies which child owns the terminal right now.
type screenID uint8

const (
	screenMain screenID = iota
	screenDashboard
	screenThread
)

// threadEventSubscriber is implemented by the concrete workspace types
// returned from AttachThread (see internal/workspace/threads.go). It is not
// part of workspace.Workspace itself — SubscribeWith is a second,
// independently stoppable subscription, distinct from the primary
// ws.Subscribe(program) pump cmd/root.go starts for the main workspace —
// so a thread's own event stream can be torn down on detach without
// disturbing the main one.
type threadEventSubscriber interface {
	SubscribeWith(send func(any)) func()
}

// threadAttachment holds everything tied to the thread currently attached
// for viewing: its own workspace, an embedded UI over that workspace, and
// the teardown funcs for both the event pump and the workspace connection
// itself.
type threadAttachment struct {
	threadID string
	name     string
	ws       workspace.Workspace
	ui       *UI
	stop     func() // stops the SubscribeWith event pump
	detach   func() // releases the attached workspace (from AttachThread)
}

// Root is the top-level tea.Model. See the package doc comment above.
type Root struct {
	com             *common.Common
	main            *UI
	dashboard       *threadsDashboard // lazily created on first ctrl+e
	dashboardDialog *dialog.Overlay   // hosts the thread-create dialog while on the dashboard screen
	thread          *threadAttachment // non-nil while attached to a thread
	active          screenID

	// pendingAttach is the ID of the thread the user most recently asked
	// to drill into (enterThreadMsg) and whose attachThreadCmd has not
	// answered yet. handleThreadAttached uses it to tell a wanted result
	// from a stale one: a request can come from the dashboard or from the
	// main screen's session panel (a click on a thread block), so "which
	// screen is active" alone cannot say whether the user is still waiting
	// — the main screen is both where a panel click starts and where an
	// abandoned dashboard request lands. Cleared when the answer arrives
	// or when the user navigates away (leaves the dashboard or a thread).
	pendingAttach string

	// send delivers messages back into the Bubble Tea event loop from
	// outside Update (the per-thread SubscribeWith pump runs its own
	// goroutine and cannot return a tea.Cmd). Wired by cmd/root.go via
	// SetSend once the tea.Program exists.
	send func(tea.Msg)

	width, height int
}

// NewRoot creates the top-level router, wrapping the main session UI.
func NewRoot(com *common.Common, initialSessionID string, continueLast bool, opts ...Option) *Root {
	return &Root{
		com:             com,
		main:            New(com, initialSessionID, continueLast, opts...),
		dashboardDialog: dialog.NewOverlay(),
		active:          screenMain,
	}
}

// SetSend wires the tea.Program's Send method so the thread event pump
// (started outside Update) can deliver messages back into the loop.
func (r *Root) SetSend(send func(tea.Msg)) { r.send = send }

// threadEventMsg tags a message from an attached thread's own event pump
// with the thread ID it came from, so a detach racing the pump's goroutine
// can be dropped instead of misrouted to whatever thread is attached next.
type threadEventMsg struct {
	threadID string
	inner    tea.Msg
}

// threadAttachedMsg delivers the result of an off-thread AttachThread call.
type threadAttachedMsg struct {
	id        string
	sessionID string
	name      string
	ws        workspace.Workspace
	detach    func()
	err       error
}

// threadActionDoneMsg delivers the result of an off-thread merge/remove
// call; both actions only need pass/fail plus a dashboard refresh.
type threadActionDoneMsg struct {
	err error
}

// threadCreatedMsg delivers the result of an off-thread CreateThread call.
type threadCreatedMsg struct {
	thread proto.Thread
	err    error
}

// Init implements tea.Model.
func (r *Root) Init() tea.Cmd {
	return r.main.Init()
}

// View implements tea.Model.
func (r *Root) View() tea.View {
	switch r.active {
	case screenThread:
		return r.thread.ui.View()
	case screenDashboard:
		return r.dashboardView()
	default:
		return r.main.View()
	}
}

// dashboardView builds the threads dashboard's screen from scratch — unlike
// screenMain/screenThread, it has no *UI of its own to delegate to. String
// hygiene (newline normalization, trailing-space trim) mirrors UI.View.
func (r *Root) dashboardView() tea.View {
	var v tea.View
	v.AltScreen = true
	v.BackgroundColor = r.com.Styles.Background
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = brand.Slug + " threads"

	canvas := uv.NewScreenBuffer(r.width, r.height)
	r.dashboard.Draw(canvas, canvas.Bounds())
	if r.dashboardDialog.HasDialogs() {
		v.Cursor = r.dashboardDialog.Draw(canvas, canvas.Bounds())
	}

	content := strings.ReplaceAll(canvas.Render(), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	v.Content = strings.Join(lines, "\n")

	return v
}

// Update implements tea.Model.
func (r *Root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return r.handleWindowSize(msg)
	case threadEventMsg:
		// A message from an attached thread's own event pump. Racing a
		// detach is expected (the pump's goroutine can't be joined
		// synchronously), so a mismatch is silently dropped, not an error.
		if r.thread != nil && r.thread.threadID == msg.threadID {
			_, cmd := r.thread.ui.Update(msg.inner)
			return r, cmd
		}
		return r, nil
	case pubsub.Event[proto.Thread]:
		var cmds []tea.Cmd
		_, cmd := r.main.Update(msg)
		cmds = append(cmds, cmd)
		if r.dashboard != nil {
			// r.main.Update above already applied this event to the shared
			// thread list cache both screens read (see threads_cache.go)
			// and dispatched any re-fetch it needs — calling
			// ApplyThreadEvent here too would apply the same event twice
			// and, while the dashboard is active, dispatch a second,
			// redundant ListThreads. Only the dashboard's own list.Items
			// need rebuilding to pick up the change.
			r.dashboard.rebuildItems()
		}
		return r, tea.Batch(cmds...)
	case tea.KeyPressMsg:
		return r.handleKeyPress(msg)
	case showThreadsDashboardMsg:
		if r.dashboard == nil {
			r.dashboard = newThreadsDashboard(r.com, &r.main.threadList)
			r.dashboard.SetSize(r.width, r.height)
		}
		cmd := r.dashboard.SetActive(true)
		r.active = screenDashboard
		return r, cmd
	case threadsLoadedMsg:
		// The shared cache (threads_cache.go) feeds the header badge and
		// dock too, so the result always goes to the main screen first,
		// regardless of which fetch — dashboard's or main's — actually
		// started it; r.main.Update applies it to the cache exactly once
		// (including any stale-generation re-dispatch). The dashboard only
		// needs its own list.Items rebuilt to reflect the now-current
		// cache.
		var cmds []tea.Cmd
		_, cmd := r.main.Update(msg)
		cmds = append(cmds, cmd)
		if r.dashboard != nil {
			r.dashboard.rebuildItems()
		}
		return r, tea.Batch(cmds...)
	case enterThreadMsg:
		r.pendingAttach = msg.id
		return r, r.attachThreadCmd(msg.id, msg.sessionID, msg.name)
	case leaveThreadRequestedMsg:
		// The Back button at the top of a drilled-in thread. The thread's
		// embedded UI raises this rather than acting itself: the
		// attachment (workspace + event pump) belongs to the router.
		if r.active != screenThread {
			return r, nil
		}
		return r, r.leaveThreadToMain()
	case threadAttachedMsg:
		return r.handleThreadAttached(msg)
	case mergeThreadMsg:
		return r, r.mergeThreadCmd(msg.id)
	case removeThreadMsg:
		return r, r.removeThreadCmd(msg.id)
	case cancelDelegationMsg:
		return r, r.cancelDelegationCmd(msg.id, msg.kind)
	case leaveDashboardMsg:
		// The dashboard's Back button. Same transition esc takes (see
		// handleKeyPress), raised as a message because the dashboard does
		// not own the screen stack — the router does.
		if r.active != screenDashboard {
			return r, nil
		}
		r.active = screenMain
		r.pendingAttach = ""
		return r, r.dashboard.SetActive(false)
	case openThreadCreateMsg:
		r.dashboardDialog.OpenDialog(dialog.NewThreadCreate(r.com))
		return r, nil
	case confirmRemoveThreadMsg:
		r.dashboardDialog.OpenDialog(dialog.NewThreadRemoveConfirm(r.com, msg.id, msg.name))
		return r, nil
	case threadActionDoneMsg:
		if msg.err != nil {
			return r, util.ReportError(msg.err)
		}
		if r.dashboard != nil {
			return r, r.dashboard.Refresh()
		}
		return r, nil
	case threadCreatedMsg:
		if msg.err != nil {
			return r, util.ReportError(msg.err)
		}
		if r.dashboard != nil {
			return r, r.dashboard.ApplyThreadEvent(pubsub.Event[proto.Thread]{
				Type:    pubsub.CreatedEvent,
				Payload: msg.thread,
			})
		}
		return r, nil
	case util.InfoMsg:
		// Errors from thread actions (attach/merge/remove/create) surface
		// here. Known limitation: the dashboard and thread screens have no
		// status line of their own yet, so these only become visible once
		// the user returns to the main screen.
		_, cmd := r.main.Update(msg)
		return r, cmd
	default:
		// Results of work the main screen started come back to the main
		// screen, whatever is on top by the time they land. Routing them
		// by active screen instead delivers them to the thread's embedded
		// UI (or, on the dashboard, nowhere at all) — and a result that
		// never arrives leaves the fetch that produced it marked
		// in-flight forever, so the main screen's threads panel and
		// header badge stop refreshing for good. That is the stale panel:
		// drill into a thread once while a refresh is out, and the panel
		// behind you never updates again.
		if _, ok := msg.(mainScreenMsg); ok {
			_, cmd := r.main.Update(msg)
			return r, cmd
		}
		// A result addressed to the UI that asked for it. Reading the
		// pointer here is on the Update goroutine, the only place a *UI
		// may be touched — the dispatching closure only ever carried it,
		// never dereferenced it.
		if owned, ok := msg.(uiOwnedMsg); ok {
			if owner := owned.ownerUI(); owner != nil {
				_, cmd := owner.Update(msg)
				return r, cmd
			}
		}
		switch msg.(type) {
		case spinner.TickMsg, spin.StepMsg, chatWarmMsg, scrollbarHideMsg, sidebarScrollbarHideMsg:
			// Animation ticks keep themselves alive: the handler that
			// receives one returns the command that schedules the next.
			// Route one by active screen and the loop does not stall, it
			// dies — the main screen's panel spinners freeze mid-frame and
			// nothing re-arms them, since syncPanelSpinner only fires on
			// the stopped→spinning edge and the tick that would have
			// cleared isSpinning never arrived. Drilling into a thread
			// once was enough to freeze the panel behind you for the rest
			// of the session. Both message types are id-stamped and every
			// handler drops ticks that aren't its own, so broadcasting to
			// each screen that owns an animation is safe.
			//
			// The chat's resize-settle warm step and the scrollbar hide
			// timers are the same shape: a one-shot the main screen arms
			// (handleWindowSize broadcasts WindowSizeMsg to it even while a
			// thread is on top) that must come back to the UI that armed it.
			// Routed by active screen, a warm step that fired while a
			// thread was open went to the thread's UI, and the main chat
			// stayed in its "resizing" state for good — the state in which
			// it never draws its scrollbar. Each of these is owner-tagged,
			// so broadcasting is safe.
			var cmds []tea.Cmd
			_, cmd := r.main.Update(msg)
			cmds = append(cmds, cmd)
			if r.thread != nil {
				_, cmd := r.thread.ui.Update(msg)
				cmds = append(cmds, cmd)
			}
			return r, tea.Batch(cmds...)
		}
		switch r.active {
		case screenThread:
			if r.thread != nil {
				_, cmd := r.thread.ui.Update(msg)
				return r, cmd
			}
			return r, nil
		case screenDashboard:
			return r.handleDashboardMsg(msg)
		default:
			_, cmd := r.main.Update(msg)
			return r, cmd
		}
	}
}

// handleWindowSize stores the new terminal size and broadcasts it to every
// screen that currently exists, so whichever one becomes active next is
// already sized correctly.
func (r *Root) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	r.width, r.height = msg.Width, msg.Height

	var cmds []tea.Cmd
	_, cmd := r.main.Update(msg)
	cmds = append(cmds, cmd)
	if r.dashboard != nil {
		r.dashboard.SetSize(msg.Width, msg.Height)
	}
	if r.thread != nil {
		_, cmd := r.thread.ui.Update(msg)
		cmds = append(cmds, cmd)
	}
	return r, tea.Batch(cmds...)
}

// handleKeyPress routes a key press according to which screen is active.
// "Go back" keys are checked first so they always win over screen-local
// bindings, per AGENTS.md's "dialog messages are intercepted first" —
// applied here one level up, to screen navigation.
func (r *Root) handleKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	toggle := key.Matches(msg, r.main.KeyMap().Threads)

	if r.active != screenMain {
		switch {
		case toggle && r.active == screenDashboard:
			r.active = screenMain
			r.pendingAttach = ""
			r.dashboard.SetActive(false)
			return r, nil
		case toggle && r.active == screenThread:
			return r, r.leaveThread()
		case key.Matches(msg, dialog.CloseKey) && r.active == screenDashboard && !r.dashboardDialog.HasDialogs():
			r.active = screenMain
			r.pendingAttach = ""
			r.dashboard.SetActive(false)
			return r, nil
		case r.active == screenThread && key.Matches(msg, r.main.KeyMap().Chat.ExitChildSession) && !r.thread.ui.viewingChildSession():
			// alt+up at the top of a drilled-in thread (no child session of
			// its own to step out of first) returns straight to the main
			// screen, not the dashboard — mirrors leaveThread's teardown but
			// skips the dashboard-refresh bits, since the dashboard may not
			// even be open. Forwarding this key into the thread's embedded
			// UI would otherwise be a no-op (its own navStack is empty), so
			// alt+up did nothing here before this check existed.
			return r, r.leaveThreadToMain()
		}
	}

	switch r.active {
	case screenDashboard:
		return r.handleDashboardKey(msg)
	case screenThread:
		_, cmd := r.thread.ui.Update(msg)
		return r, cmd
	default:
		_, cmd := r.main.Update(msg)
		return r, cmd
	}
}

// handleDashboardMsg routes messages the dashboard screen cares about but
// that are not key presses — mouse input, which the screen's toolbar,
// filter tabs and table all respond to. Anything else is dropped, as it
// was before the dashboard grew a mouse surface.
func (r *Root) handleDashboardMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	if r.dashboard == nil {
		return r, nil
	}
	// An open dialog owns the pointer: clicking "through" a modal onto the
	// toolbar behind it would act on a screen the user cannot see.
	if r.dashboardDialog.HasDialogs() {
		if _, ok := msg.(tea.MouseMsg); ok {
			action := r.dashboardDialog.Update(msg)
			return r, r.handleDashboardDialogAction(action)
		}
		return r, nil
	}

	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		_, cmd := r.dashboard.HandleMouseClick(msg)
		return r, cmd
	case tea.MouseMotionMsg:
		r.dashboard.HandleMouseMotion(msg)
		return r, nil
	case tea.MouseWheelMsg:
		r.dashboard.HandleMouseWheel(msg)
		return r, nil
	}
	return r, nil
}

// handleDashboardKey routes a key press while the dashboard screen is
// active: the create-thread dialog wins when open, otherwise the dashboard
// list itself handles it.
func (r *Root) handleDashboardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if r.dashboardDialog.HasDialogs() {
		action := r.dashboardDialog.Update(msg)
		return r, r.handleDashboardDialogAction(action)
	}
	_, cmd := r.dashboard.HandleKey(msg)
	return r, cmd
}

// handleDashboardDialogAction mirrors UI.handleDialogMsg's shape, but only
// for the actions the dashboard's own dialogs (thread-create, thread-remove
// confirmation) can produce.
func (r *Root) handleDashboardDialogAction(action dialog.Action) tea.Cmd {
	switch action := action.(type) {
	case dialog.ActionClose:
		r.dashboardDialog.CloseFrontDialog()
	case dialog.ActionCmd:
		return action.Cmd
	case dialog.ActionCreateThread:
		r.dashboardDialog.CloseFrontDialog()
		return r.createThreadCmd(action.Name, action.Goal)
	case dialog.ActionRemoveThreadConfirmed:
		r.dashboardDialog.CloseFrontDialog()
		return r.removeThreadCmd(action.ID)
	}
	return nil
}

// attachThreadCmd calls AttachThread off-thread. Per AGENTS.md, model state
// is never touched inside a command closure — only locals are captured.
func (r *Root) attachThreadCmd(id, sessionID, name string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		attached, detach, err := ws.AttachThread(ctx, id)
		return threadAttachedMsg{id: id, sessionID: sessionID, name: name, ws: attached, detach: detach, err: err}
	}
}

// handleThreadAttached applies the result of attachThreadCmd. On success it
// builds the thread's own embedded UI, starts its event pump, and switches
// the active screen; on failure it reports the error once the user is back
// on the main screen (see the util.InfoMsg case in Update).
func (r *Root) handleThreadAttached(msg threadAttachedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return r, util.ReportError(msg.err)
	}

	// The result is wanted only if it answers the request the user is
	// still waiting on — the most recent enterThreadMsg (from the dashboard
	// or the main screen's session panel; see pendingAttach) — or is a
	// duplicate response for the thread already attached (e.g. two Enter
	// presses on the same dashboard row). Anything else means they've moved
	// on since asking (left the dashboard, asked for a different thread):
	// release what was just attached instead of yanking the screen onto a
	// thread nobody is waiting for. pendingAttach holds only the latest
	// request, so an older request for a different thread that lands later
	// is stale by construction.
	current := msg.id == r.pendingAttach ||
		(r.active == screenThread && r.thread != nil && r.thread.threadID == msg.id)
	if !current {
		if msg.detach == nil {
			return r, nil
		}
		detach := msg.detach
		return r, func() tea.Msg {
			detach()
			return nil
		}
	}
	r.pendingAttach = ""

	com := common.DefaultCommon(r.com.Context(), msg.ws)
	childUI := New(com, msg.sessionID, false, WithEmbedded(), WithBreadcrumbRoot(msg.name))

	stop := func() {}
	if sub, ok := msg.ws.(threadEventSubscriber); ok {
		id := msg.id
		stop = sub.SubscribeWith(func(m any) {
			if r.send != nil {
				r.send(threadEventMsg{threadID: id, inner: m})
			}
		})
	}

	// Tear down whatever was attached before installing this one — a
	// second threadAttachedMsg for the same thread (double Enter) must not
	// leak the previous attachment's pump goroutine or workspace.
	detachCmd := r.detachThread()

	r.thread = &threadAttachment{
		threadID: msg.id,
		name:     msg.name,
		ws:       msg.ws,
		ui:       childUI,
		stop:     stop,
		detach:   msg.detach,
	}
	r.active = screenThread

	var cmds []tea.Cmd
	cmds = append(cmds, detachCmd)
	cmds = append(cmds, childUI.Init())
	_, cmd := childUI.Update(tea.WindowSizeMsg{Width: r.width, Height: r.height})
	cmds = append(cmds, cmd)
	return r, tea.Batch(cmds...)
}

// detachThread stops the attached thread's event pump and releases its
// workspace, leaving r.active for the caller to set. Shared by leaveThread
// and leaveThreadToMain, which differ only in where they land and whether
// the dashboard needs a refresh.
//
// Both halves of the teardown run inside the returned tea.Cmd rather than
// here, because this runs on the Bubble Tea event loop, inside Update.
// detach may do IO (e.g. shutting down a client connection), and stop waits
// for the SubscribeWith pump goroutine to exit — which deadlocks the whole
// TUI if done here: that goroutine delivers its events through
// tea.Program.Send, which parks until the event loop takes the message, and
// the event loop is precisely what is waiting for it. Cancelling the
// subscription does not break the cycle, since the pump is parked in the
// send rather than at its select. Dropping r.thread now is what makes this
// safe to defer: any event the pump still delivers no longer matches an
// attachment and is discarded (see the threadEventMsg case in Update).
func (r *Root) detachThread() tea.Cmd {
	if r.thread == nil {
		return nil
	}
	thread := r.thread
	r.thread = nil

	if thread.stop == nil && thread.detach == nil {
		return nil
	}
	return func() tea.Msg {
		if thread.stop != nil {
			thread.stop()
		}
		if thread.detach != nil {
			thread.detach()
		}
		return nil
	}
}

// leaveThread tears down the attached thread and returns to the dashboard.
func (r *Root) leaveThread() tea.Cmd {
	cmd := r.detachThread()
	r.active = screenDashboard
	r.pendingAttach = ""

	var cmds []tea.Cmd
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	if r.dashboard != nil {
		// The thread may have changed status while attached (e.g.
		// completed); invalidate so the dashboard doesn't sit on a stale
		// row until the TTL backstop catches up.
		r.dashboard.cache.invalidate()
		cmds = append(cmds, r.dashboard.Refresh())
	}
	return tea.Batch(cmds...)
}

// leaveThreadToMain tears down the attached thread and returns straight to
// the main screen (alt+up at the top of a drilled-in thread — see
// handleKeyPress), rather than the threads dashboard leaveThread goes to.
// Skips the dashboard-refresh bits: the dashboard may not even be open, and
// if it is, its own TTL backstop reconciles the thread's status change.
func (r *Root) leaveThreadToMain() tea.Cmd {
	cmd := r.detachThread()
	r.active = screenMain
	r.pendingAttach = ""
	return cmd
}

// mergeThreadCmd calls MergeThread off-thread.
func (r *Root) mergeThreadCmd(id string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		_, err := ws.MergeThread(ctx, id)
		return threadActionDoneMsg{err: err}
	}
}

// removeThreadCmd calls RemoveThread off-thread.
func (r *Root) removeThreadCmd(id string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		err := ws.RemoveThread(ctx, id, proto.RemoveThreadOptions{})
		return threadActionDoneMsg{err: err}
	}
}

// cancelDelegationCmd calls CancelTask or CancelThread off-thread,
// depending on kind — reuses threadActionDoneMsg so a successful cancel
// gets the same dashboard refresh merge/remove already trigger.
func (r *Root) cancelDelegationCmd(id, kind string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		var err error
		if proto.ThreadKind(kind) == proto.ThreadKindThread {
			err = ws.CancelThread(ctx, id, "cancelled from panel")
		} else {
			err = ws.CancelTask(ctx, id, "cancelled from panel")
		}
		return threadActionDoneMsg{err: err}
	}
}

// createThreadCmd calls CreateThread off-thread with the dialog's validated
// input, attributing the thread to the session it was started from.
//
// That attribution is what makes the thread's completion come back: a
// thread reports to its parent session when it finishes, and one created
// without a parent has nobody to tell, leaving its result discoverable
// only by going and looking. Empty when the dashboard is open without a
// session, which is the same "nobody is waiting" case the CLI creates.
func (r *Root) createThreadCmd(name, goal string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	parentSessionID := ""
	if r.main != nil && r.main.hasSession() {
		parentSessionID = r.main.sess.current.ID
	}
	return func() tea.Msg {
		thread, err := ws.CreateThread(ctx, proto.CreateThreadRequest{
			Name:            name,
			Goal:            goal,
			ParentSessionID: parentSessionID,
		})
		return threadCreatedMsg{thread: thread, err: err}
	}
}

// Cleanup releases the attached thread's resources, if any. Called once by
// cmd/root.go after program.Run() returns — best-effort, since there is no
// further chance to surface an error through the (now-stopped) TUI.
func (r *Root) Cleanup() {
	if r.thread == nil {
		return
	}
	if r.thread.stop != nil {
		r.thread.stop()
	}
	if r.thread.detach != nil {
		r.thread.detach()
	}
	r.thread = nil
}

// uiOwnedMsg marks the result of work a specific *UI started, so Root can
// hand it back to that UI rather than to whichever screen happens to be on
// top when it lands. Every one of these clears an in-flight or loading
// flag, and each *UI (the main screen's and an attached thread's) keeps
// its own: routed by active screen instead, a probe still in flight when
// the user opened the dashboard or drilled into a thread had its result
// dropped — the dashboard's own handler forwards nothing but mouse events
// — and the flag it would have cleared stayed set for the rest of the
// session, so that refresh never ran again. That is a permanently stale
// busy state, prompt queue, LSP panel, sessions dialog that will not open,
// or a session that never finishes loading.
//
// Embed uiOwned and set owner at dispatch to claim this.
type uiOwnedMsg interface{ ownerUI() *UI }

// uiOwned is the embeddable implementation of uiOwnedMsg.
type uiOwned struct{ owner *UI }

func (o uiOwned) ownerUI() *UI { return o.owner }

// mainScreenMsg marks a message that belongs to the main session screen no
// matter which screen is currently on top — the result of a refresh only
// the main screen ever starts. Root delivers these to r.main directly
// rather than to the active screen; see the reasoning in Update.
//
// Embed mainScreenOwned in the message type to claim this.
type mainScreenMsg interface{ isMainScreenMsg() }

// mainScreenOwned is the embeddable implementation of mainScreenMsg.
type mainScreenOwned struct{}

func (mainScreenOwned) isMainScreenMsg() {}

// Compile-time proof that each main-owned message actually claims the
// interface. Embedding a struct whose method shares its own type name
// silently fails to promote that method, so without these the marker can
// look present and do nothing.
var (
	_ mainScreenMsg = threadDockActivityLoadedMsg{}

	_ uiOwnedMsg = busyStateMsg{}
	_ uiOwnedMsg = promptQueueMsg{}
	_ uiOwnedMsg = lspStatesMsg{}
	_ uiOwnedMsg = sessionsLoadedMsg{}
	_ uiOwnedMsg = loadSessionMsg{}
)
