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
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/rave-soft/braid/internal/workspace"
)

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
	SubscribeWith(send func(tea.Msg)) func()
}

// threadAttachment holds everything tied to the thread currently attached
// for viewing: its own workspace, an embedded UI over that workspace, and
// the teardown funcs for both the event pump and the workspace connection
// itself.
type threadAttachment struct {
	threadID string
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

	// send delivers messages back into the Bubble Tea event loop from
	// outside Update (the per-thread SubscribeWith pump runs its own
	// goroutine and cannot return a tea.Cmd). Wired by cmd/root.go via
	// SetSend once the tea.Program exists.
	send func(tea.Msg)

	width, height int
}

// NewRoot creates the top-level router, wrapping the main session UI.
func NewRoot(com *common.Common, initialSessionID string, continueLast bool) *Root {
	return &Root{
		com:             com,
		main:            New(com, initialSessionID, continueLast),
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
	v.WindowTitle = "braid threads"

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
			cmds = append(cmds, r.dashboard.ApplyThreadEvent(msg))
		}
		return r, tea.Batch(cmds...)
	case tea.KeyPressMsg:
		return r.handleKeyPress(msg)
	case showThreadsDashboardMsg:
		if r.dashboard == nil {
			r.dashboard = newThreadsDashboard(r.com)
			r.dashboard.SetSize(r.width, r.height)
		}
		cmd := r.dashboard.SetActive(true)
		r.active = screenDashboard
		return r, cmd
	case threadsLoadedMsg:
		if r.dashboard == nil {
			return r, nil
		}
		return r, tea.Batch(r.dashboard.ApplyThreadsLoaded(msg)...)
	case enterThreadMsg:
		return r, r.attachThreadCmd(msg.id, msg.sessionID)
	case threadAttachedMsg:
		return r.handleThreadAttached(msg)
	case mergeThreadMsg:
		return r, r.mergeThreadCmd(msg.id)
	case removeThreadMsg:
		return r, r.removeThreadCmd(msg.id)
	case cancelDelegationMsg:
		return r, r.cancelDelegationCmd(msg.id, msg.kind)
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
		switch r.active {
		case screenThread:
			if r.thread != nil {
				_, cmd := r.thread.ui.Update(msg)
				return r, cmd
			}
			return r, nil
		case screenDashboard:
			return r, nil
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
			r.dashboard.SetActive(false)
			return r, nil
		case toggle && r.active == screenThread:
			return r, r.leaveThread()
		case key.Matches(msg, dialog.CloseKey) && r.active == screenDashboard && !r.dashboardDialog.HasDialogs():
			r.active = screenMain
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
func (r *Root) attachThreadCmd(id, sessionID string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		attached, detach, err := ws.AttachThread(ctx, id)
		return threadAttachedMsg{id: id, sessionID: sessionID, ws: attached, detach: detach, err: err}
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

	com := common.DefaultCommon(r.com.Context(), msg.ws)
	childUI := New(com, msg.sessionID, false, WithEmbedded())

	stop := func() {}
	if sub, ok := msg.ws.(threadEventSubscriber); ok {
		id := msg.id
		stop = sub.SubscribeWith(func(m tea.Msg) {
			if r.send != nil {
				r.send(threadEventMsg{threadID: id, inner: m})
			}
		})
	}

	r.thread = &threadAttachment{
		threadID: msg.id,
		ws:       msg.ws,
		ui:       childUI,
		stop:     stop,
		detach:   msg.detach,
	}
	r.active = screenThread

	var cmds []tea.Cmd
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

	var cmds []tea.Cmd
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	if r.dashboard != nil {
		// The thread may have changed status while attached (e.g.
		// completed); invalidate so the dashboard doesn't sit on a stale
		// row until the TTL backstop catches up.
		r.dashboard.cache.invalidateThreads()
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
		if thread.Kind(kind) == thread.KindThread {
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
		parentSessionID = r.main.session.ID
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
