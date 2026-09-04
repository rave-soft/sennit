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
	"fmt"
	"reflect"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	fimage "github.com/rave-soft/sennit/internal/ui/image"
	"github.com/rave-soft/sennit/internal/ui/threads"
	"github.com/rave-soft/sennit/internal/ui/uimsg"
	"github.com/rave-soft/sennit/internal/ui/util"
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
	ui       *UI
	stop     func() // stops the SubscribeWith event pump
	detach   func() // releases the attached workspace (from AttachThread)
}

// threadAttachmentState owns the requested and currently attached thread.
// Its release method removes the attachment before its potentially blocking
// teardown runs in a command, so late events cannot reach a replacement.
type threadAttachmentState struct {
	thread    *threadAttachment
	pendingID string
}

func (s *threadAttachmentState) release() tea.Cmd {
	if s.thread == nil {
		return nil
	}
	thread := s.thread
	s.thread = nil
	return func() tea.Msg {
		cancelThreadQuestion(thread)
		stopThreadTurnTimer(thread)
		if thread.stop != nil {
			thread.stop()
		}
		if thread.detach != nil {
			thread.detach()
		}
		return nil
	}
}

func (s *threadAttachmentState) cleanup() {
	if s.thread == nil {
		return
	}
	cancelThreadQuestion(s.thread)
	stopThreadTurnTimer(s.thread)
	if s.thread.stop != nil {
		s.thread.stop()
	}
	if s.thread.detach != nil {
		s.thread.detach()
	}
	s.thread = nil
}

// cancelThreadQuestion cancels any question still pending on the detached
// thread's own workspace. Destroying the embedded window (thread.ui) drops
// its open QuestionForm without ever calling question.Service.Cancel, and
// the question tool that raised it is blocked in Ask with no timeout — see
// forwardQuestions. Detaching used to leave it stuck there forever, since
// nothing else in the tool's lifetime ever answers or cancels it.
func cancelThreadQuestion(thread *threadAttachment) {
	if thread == nil || thread.ui == nil || thread.ui.com == nil || thread.ui.com.Workspace == nil {
		return
	}
	thread.ui.com.Workspace.QuestionCancel()
}

// stopThreadTurnTimer stops the turn-elapsed clock for the detached
// thread's own session, if one was running. common.StartTurn/StopTurn are
// keyed by session ID in a package-level table (see internal/ui/common's
// timer.go) shared by every *UI in this process, including this thread's
// embedded one; destroying that embedded UI does not touch that table on
// its own, so a turn still in flight when the person detaches would sit
// there marked "started" forever, and Elapsed(sessionID) would go on
// reporting time for a turn nobody is displaying or ever will finish
// watching.
func stopThreadTurnTimer(thread *threadAttachment) {
	if thread == nil || thread.ui == nil || thread.ui.sess.current == nil {
		return
	}
	common.StopTurn(thread.ui.sess.current.ID)
}

// Root is the top-level tea.Model. See the package doc comment above.
type Root struct {
	com             *common.Common
	main            *UI
	dashboard       *threads.Dashboard // lazily created on first ctrl+e
	dashboardDialog *dialog.Overlay    // hosts the thread-create dialog while on the dashboard screen
	attachment      threadAttachmentState
	active          screenID

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

// threadAttachedMsg delivers the result of an off-thread ActivateThread +
// AttachThread call.
type threadAttachedMsg struct {
	id        string
	sessionID string
	name      string
	ws        common.Workspace
	detach    func()
	err       error
	// activateErr is set when ActivateThread failed to revive the thread.
	// It does not abort the attach — msg.ws is still the read-only view
	// AttachThread fell back to — but the reason is worth explaining to
	// the user at open time (as a warning, not an error: the common case
	// is a merged/merging thread, whose read-only state is correct and
	// permanent) rather than leaving them to discover it only once they
	// try to type and are refused.
	activateErr error
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
		return r.attachment.thread.ui.View()
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
		if r.attachment.thread != nil && r.attachment.thread.threadID == msg.threadID {
			_, cmd := r.attachment.thread.ui.Update(msg.inner)
			return r, cmd
		}
		return r, nil
	case ownedMsg:
		// Checked ahead of the generic uiOwnedMsg branch in default: below
		// (ownedMsg embeds uiOwned and would otherwise match there too),
		// because that branch forwards msg itself, and forwarding the
		// envelope unopened would hand the owner a type its own Update
		// never registered a case for. See ownedMsg's doc for why a stale
		// owner (e.g. a detached thread's UI) is safe to forward to
		// anyway.
		if owner := msg.ownerUI(); owner != nil {
			_, cmd := owner.Update(msg.inner)
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
			r.dashboard.RebuildItems()
		}
		return r, tea.Batch(cmds...)
	case tea.KeyPressMsg:
		return r.handleKeyPress(msg)
	case showThreadsDashboardMsg:
		if r.dashboard == nil {
			r.dashboard = threads.New(r.com, &r.main.threadList)
			r.dashboard.SetSize(r.width, r.height)
		}
		cmd := r.dashboard.SetActive(true)
		r.active = screenDashboard
		return r, cmd
	case threads.LoadedMsg:
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
			r.dashboard.RebuildItems()
		}
		return r, tea.Batch(cmds...)
	case threads.EnterMsg:
		r.attachment.pendingID = msg.ID
		return r, r.attachThreadCmd(msg.ID, msg.SessionID, msg.Name)
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
	case threads.MergeMsg:
		return r, r.mergeThreadCmd(msg.ID)
	case threads.RemoveMsg:
		return r, r.removeThreadCmd(msg.ID)
	case threads.CancelDelegationMsg:
		return r, r.cancelDelegationCmd(msg.ID, msg.Kind)
	case threads.LeaveMsg:
		// The dashboard's Back button. Same transition esc takes (see
		// handleKeyPress), raised as a message because the dashboard does
		// not own the screen stack — the router does.
		if r.active != screenDashboard {
			return r, nil
		}
		r.active = screenMain
		r.attachment.pendingID = ""
		return r, r.dashboard.SetActive(false)
	case threads.OpenCreateMsg:
		r.dashboardDialog.OpenDialog(dialog.NewThreadCreate(r.com))
		return r, nil
	case threads.ConfirmRemoveMsg:
		r.dashboardDialog.OpenDialog(dialog.NewThreadRemoveConfirm(r.com, msg.ID, msg.Name))
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
		if _, ok := msg.(uimsg.MainScreenMsg); ok {
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
		case spinner.TickMsg, spin.StepMsg, chatlist.WarmMsg, chatlist.ScrollbarHideMsg, sidebarScrollbarHideMsg:
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
			if r.attachment.thread != nil {
				_, cmd := r.attachment.thread.ui.Update(msg)
				cmds = append(cmds, cmd)
			}
			return r, tea.Batch(cmds...)
		}
		// Everything left is not an owned result, a MainScreenMsg, or an
		// animation tick — raw terminal input (mouse, wheel, paste; key
		// presses are handled above by handleKeyPress) is the only kind of
		// message that legitimately depends on which screen is on top, so
		// that is the only thing routed by r.active from here on.
		switch msg.(type) {
		case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, common.CoalescedWheelMsg, tea.PasteMsg:
			switch r.active {
			case screenThread:
				if r.attachment.thread != nil {
					_, cmd := r.attachment.thread.ui.Update(msg)
					return r, cmd
				}
				return r, nil
			case screenDashboard:
				return r.handleDashboardMsg(msg)
			}
		}
		// Not input, and not claimed by anything above: a backend event
		// with no owner of its own (see the uiOwnedMsg doc's "no owner"
		// case), or a message type nothing here recognizes. Belongs to
		// the main screen regardless of which screen is on top — routing
		// it by active screen instead is the original bug this whole
		// mechanism exists to fix.
		_, cmd := r.main.Update(msg)
		return r, cmd
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
	if r.attachment.thread != nil {
		_, cmd := r.attachment.thread.ui.Update(msg)
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
			r.attachment.pendingID = ""
			r.dashboard.SetActive(false)
			return r, nil
		case toggle && r.active == screenThread:
			return r, r.leaveThread()
		case key.Matches(msg, dialog.CloseKey) && r.active == screenDashboard && !r.dashboardDialog.HasDialogs():
			r.active = screenMain
			r.attachment.pendingID = ""
			r.dashboard.SetActive(false)
			return r, nil
		case r.active == screenThread && key.Matches(msg, r.main.KeyMap().Chat.ExitChildSession) && !r.attachment.thread.ui.sess.viewingChildSession():
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
		_, cmd := r.attachment.thread.ui.Update(msg)
		return r, cmd
	default:
		_, cmd := r.main.Update(msg)
		return r, cmd
	}
}

// handleDashboardMsg routes the dashboard screen's own input: mouse, wheel
// and paste, which the screen's toolbar, filter tabs, table and
// create-thread dialog (open or not) all respond to. Root.Update's
// fallback now routes by active screen only for these message types —
// everything else (a backend event, an owned async result) already went
// to r.main before handleDashboardMsg is ever called, so this no longer
// classifies input itself.
func (r *Root) handleDashboardMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	if r.dashboard == nil {
		return r, nil
	}

	// An open dialog owns the pointer: clicking "through" a modal onto the
	// toolbar behind it would act on a screen the user cannot see. Paste is
	// forwarded too — the thread-create dialog's text inputs sit behind
	// this same guard, and without it a pasted multi-line goal silently
	// went nowhere while the main screen's dialogs kept receiving paste
	// normally.
	if r.dashboardDialog.HasDialogs() {
		action := r.dashboardDialog.Update(msg)
		return r, r.handleDashboardDialogAction(action)
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
	case common.CoalescedWheelMsg:
		// The input filter (cmd/root.go) rewrites every raw wheel event
		// into this before it reaches here, so the tea.MouseWheelMsg case
		// above is dead in production — mirror mouse.go's main-screen
		// handling and translate the coalesced delta back into the
		// button HandleMouseWheel expects.
		button := tea.MouseWheelDown
		if msg.DeltaY < 0 {
			button = tea.MouseWheelUp
		}
		if msg.DeltaY != 0 {
			r.dashboard.HandleMouseWheel(tea.MouseWheelMsg{
				X:      msg.Mouse.X,
				Y:      msg.Mouse.Y,
				Button: button,
			})
		}
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

// attachThreadCmd calls ActivateThread then AttachThread off-thread. Per
// AGENTS.md, model state is never touched inside a command closure — only
// locals are captured.
//
// ActivateThread revives the thread's own isolated workspace if it is not
// already running — this is the drill-in path (the user pressed Enter to
// open a thread), which is exactly the caller that wants reactivation, as
// opposed to e.g. the dock's background activity refresh, which must
// never trigger a spawn as a side effect of drawing itself. A failure here
// does not abort: AttachThread still runs and hands back its read-only
// fallback, so the user gets *something* to look at, with the reason
// carried in activateErr for handleThreadAttached to explain to them —
// most commonly a merged/merging thread, for which read-only is the
// correct and permanent state, not a failure.
func (r *Root) attachThreadCmd(id, sessionID, name string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		_, activateErr := ws.ActivateThread(ctx, id)
		attached, detach, err := ws.AttachThread(ctx, id)
		return threadAttachedMsg{id: id, sessionID: sessionID, name: name, ws: attached, detach: detach, err: err, activateErr: activateErr}
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
	// still waiting on — the most recent threads.EnterMsg (from the dashboard
	// or the main screen's session panel; see pendingAttach) — or is a
	// duplicate response for the thread already attached (e.g. two Enter
	// presses on the same dashboard row). Anything else means they've moved
	// on since asking (left the dashboard, asked for a different thread):
	// release what was just attached instead of yanking the screen onto a
	// thread nobody is waiting for. pendingAttach holds only the latest
	// request, so an older request for a different thread that lands later
	// is stale by construction.
	current := msg.id == r.attachment.pendingID ||
		(r.active == screenThread && r.attachment.thread != nil && r.attachment.thread.threadID == msg.id)
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
	r.attachment.pendingID = ""

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

	r.attachment.thread = &threadAttachment{
		threadID: msg.id,
		name:     msg.name,
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
	if msg.activateErr != nil {
		// The thread still opened (read-only, via AttachThread's
		// fallback). This is not a failure to flag — the most common
		// reason is a merged/merging thread, whose read-only state is
		// correct and permanent, not something gone wrong — so it is a
		// warning explaining what they're looking at, not an error, and
		// it says so rather than leaving them to discover it only once
		// they try to type.
		cmds = append(cmds, util.ReportWarn(fmt.Sprintf("Thread opened read-only: %s", msg.activateErr)))
	}
	return r, tea.Batch(cmds...)
}

// detachThread releases the attached thread without blocking the event loop.
// The attachment state owns the teardown details and removes the attachment
// before the returned command runs, so late pump events are discarded.
func (r *Root) detachThread() tea.Cmd {
	return r.attachment.release()
}

// leaveThread tears down the attached thread and returns to the dashboard.
// A thread reached via enterThreadMsg (e.g. the main screen's session panel)
// never had a dashboard constructed for it — only showThreadsDashboardMsg
// does that — so this lazily builds one here too, the same way that case
// does, before switching screens; View()'s screenDashboard case draws
// r.dashboard unconditionally and would otherwise panic on a nil dashboard.
func (r *Root) leaveThread() tea.Cmd {
	cmd := r.detachThread()
	built := r.dashboard == nil
	if built {
		r.dashboard = threads.New(r.com, &r.main.threadList)
		r.dashboard.SetSize(r.width, r.height)
	}
	r.active = screenDashboard
	r.attachment.pendingID = ""

	var cmds []tea.Cmd
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	if built {
		cmds = append(cmds, r.dashboard.SetActive(true))
	}
	// The thread may have changed status while attached (e.g.
	// completed); invalidate so the dashboard doesn't sit on a stale
	// row until the TTL backstop catches up.
	r.dashboard.InvalidateCache()
	cmds = append(cmds, r.dashboard.Refresh())
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
	r.attachment.pendingID = ""
	return cmd
}

func (r *Root) mergeThreadCmd(id string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		_, err := ws.MergeThread(ctx, id)
		return threadActionDoneMsg{err: err}
	}
}

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
	if r.main != nil && r.main.sess.hasSession() {
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
	r.attachment.cleanup()
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

// ownedMsg envelopes a message Root cannot tag by embedding uiOwned
// directly in its type — because that type is defined in a package model
// already imports (util, completions, image, dialog), and embedding there
// would need that package to import model back, closing a cycle — with
// the *UI that dispatched it. Root's ownedMsg case above unwraps it and
// forwards inner to owner, exactly the shape threadEventMsg already uses
// to forward a thread's own pump event to the thread's UI.
//
// Unlike threadEventMsg, ownedMsg identifies its target by *UI pointer
// rather than by thread ID, so it needs no "is this still the attached
// thread" check: the pointer names one specific *UI instance, not
// "whichever thread is attached right now," so a second thread attaching
// before this lands can never receive it — there is no ID to collide on,
// only a pointer to an instance that either still matters or does not. A
// stale owner (its thread since detached) is simply a *UI nobody renders
// any more; forwarding to it is wasted, not unsafe — AttachThread's own
// doc notes detach is a no-op in local mode (the thread's App outlives
// whoever is viewing it), so nothing reachable through a stale owner's
// Update is a resource that has actually been released.
//
// Construct one through ownCmd rather than by hand — it also knows how to
// carry a tea.BatchMsg through without boxing it opaquely (see its doc).
type ownedMsg struct {
	uiOwned
	inner tea.Msg
}

// ownCmd tags cmd's eventual result with owner, so Root's ownedMsg case
// delivers it back to this *UI instead of whichever screen is active when
// it lands. Used at dispatch sites for a message type defined outside
// model that therefore cannot embed uiOwned itself, and for
// applyChromeDialogAction's dialog.ActionCmd case, where the wrap site
// does not choose the message type — it's whatever the open dialog's own
// async work produces.
//
// cmd may itself be a tea.Batch(...) result — dialog.ActionCmd's Cmd
// sometimes is (see FilePicker.HandleMsg, which batches its preview-prepare
// cmd alongside its embedded bubble's own cmd) — so the result is checked
// for tea.BatchMsg one level deep and, if found, each element is re-wrapped
// individually rather than the whole batch being boxed as one opaque
// envelope Bubble Tea never learns to expand back into separate messages.
// tea.Sequence's message type is unexported and cannot be detected the
// same way, so this must never wrap a Cmd that might be Sequence-composed
// — confirmed by inspection that internal/ui/dialog uses tea.Batch only,
// never tea.Sequence.
//
// Because applyChromeDialogAction's dialog.ActionCmd case funnels cmds
// this generic (it doesn't know in advance what a dialog's own command
// will produce), the result is only wrapped when it is one of the known
// cross-package types ownedResultTypes lists — never unconditionally.
// FilePicker's own preview-prepare cmd sits in that same batch alongside
// ActionCmd{Cmd: tea.Raw(msg.Output)} (see FilePicker.handlePreviewPrepared)
// for copying preview output to the terminal: tea.RawMsg is a Bubble Tea
// runtime sentinel the Program intercepts before Update ever sees it, the
// same category as tea.Quit's message — boxing it in ownedMsg silently
// broke that terminal write (caught by
// TestCmdDriving_PreviewResultRoutedToCoveredFilePicker). Wrapping only
// what's on the list, and passing everything else through unchanged, is
// what keeps that safe while still tagging the types that do need it.
func ownCmd(owner *UI, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		return ownResult(owner, cmd())
	}
}

// ownedResultTypes is the allow-list ownResult wraps: message types
// defined outside model, dispatched via ownCmd because they cannot embed
// uiOwned themselves. Deliberately narrow — see ownCmd's doc for why a
// wrap-everything version broke a Bubble Tea runtime sentinel.
//
// This is a maintenance point, not a closed set: any future message type
// defined outside model that a dialog's async work (ActionCmd) can produce
// must be added here to get owner-routed. There is no compiler check for
// that — an omission does not fail to build, it just routes by active
// screen again, the original bug, silently. If you add a dialog whose
// HandleMsg returns ActionCmd wrapping a new outside-model type and it
// needs to reach the UI that opened the dialog while another screen is on
// top, add that type's reflect.Type here.
//
// chatlist.DelayedClickMsg is not dialog-sourced — it's Chat's own
// self-scheduled tea.Tick follow-up to a mouse-down (see HandleMouseDown),
// wrapped where mouse.go appends that cmd. It belongs on this list for the
// same reason as the others: chatlist cannot import model, so it cannot
// embed uiOwned, and the type-switch fallback inversion below would
// otherwise route it to r.main unconditionally. That would have been worse
// than the pre-inversion misroute it replaces: Chat.HandleDelayedClick
// guards on a ClickID matching the receiving Chat's own pendingClickID,
// but that counter is a small per-instance int (0, 1, 2, ...) — two
// independent Chat instances (main's and a thread's) collide on it often,
// so a delayed click misrouted to the wrong screen's chat does not
// reliably no-op; it can coincidentally match and toggle expansion or
// navigate into a child session on a screen the user never clicked.
// Discovered while auditing this fallback for the inversion, before it
// shipped — not a bug this inversion introduced, but one it would have
// made unconditional instead of a rare screen-switch-mid-delay race.
var ownedResultTypes = map[reflect.Type]bool{
	reflect.TypeFor[util.ClearStatusMsg]():                  true,
	reflect.TypeFor[completions.CompletionItemsLoadedMsg](): true,
	reflect.TypeFor[fimage.PreviewPreparedMsg]():            true,
	reflect.TypeFor[chatlist.DelayedClickMsg]():             true,
}

// ownResult wraps a single command result for ownCmd, expanding one level
// of tea.BatchMsg first — see ownCmd's doc for why — and otherwise only
// wrapping a type ownedResultTypes recognizes; anything else (a Bubble Tea
// runtime sentinel, or a message type nothing here needs to route by
// owner) is returned unchanged.
func ownResult(owner *UI, msg tea.Msg) tea.Msg {
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		wrapped := make(tea.BatchMsg, 0, len(batch))
		for _, c := range batch {
			if w := ownCmd(owner, c); w != nil {
				wrapped = append(wrapped, w)
			}
		}
		return wrapped
	}
	if !ownedResultTypes[reflect.TypeOf(msg)] {
		return msg
	}
	return ownedMsg{uiOwned: uiOwned{owner: owner}, inner: msg}
}

// Compile-time proof that each main-owned message actually claims the
// interface. Embedding a struct whose method shares its own type name
// silently fails to promote that method, so without these the marker can
// look present and do nothing.
//
// This list is not exhaustive of every owner-tagged message: a type
// defined outside model (util.ClearStatusMsg, completions.
// CompletionItemsLoadedMsg, fimage.PreviewPreparedMsg, and the
// dialog.Action* values that round-trip through applyChromeDialogAction's
// default arm) is tagged via the ownedMsg envelope at its dispatch site
// instead, since it cannot embed uiOwned without model importing back into
// a package it already imports. ownedMsg itself is asserted below; there
// is nothing further to assert per envelope-wrapped type, since the
// wrapping — not the wrapped type — is what carries the ownership tag.
var (
	_ uimsg.MainScreenMsg = threads.DockActivityLoadedMsg{}

	_ uiOwnedMsg = busyStateMsg{}
	_ uiOwnedMsg = promptQueueMsg{}
	_ uiOwnedMsg = lspStatesMsg{}
	_ uiOwnedMsg = sessionsLoadedMsg{}
	_ uiOwnedMsg = loadSessionMsg{}
	_ uiOwnedMsg = agentsLoadedMsg{}
	_ uiOwnedMsg = shellResultMsg{}
	_ uiOwnedMsg = agentRunSubmittedMsg{}
	_ uiOwnedMsg = sendMessageErrorMsg{}
	_ uiOwnedMsg = createSessionMsg{}
	_ uiOwnedMsg = bangSessionCreatedMsg{}
	_ uiOwnedMsg = cancelTimerExpiredMsg{}
	_ uiOwnedMsg = ownedMsg{}

	// Tagged in the second pass (piece 2): every remaining message type
	// defined in model that a *UI command can dispatch. See ownedMsg's doc
	// and ownedResultTypes above for the disjoint set — types defined
	// outside model — that cannot embed uiOwned and are tagged at their
	// dispatch site instead.
	_ uiOwnedMsg = requestSessionLoad{}
	_ uiOwnedMsg = sessionFilesUpdatesMsg{}
	_ uiOwnedMsg = sendMessageMsg{}
	_ uiOwnedMsg = sendPendingQueueMsg{}
	_ uiOwnedMsg = userCommandsLoadedMsg{}
	_ uiOwnedMsg = mcpStateChangedMsg{}
	_ uiOwnedMsg = mcpPromptsLoadedMsg{}
	_ uiOwnedMsg = promptHistoryLoadedMsg{}
	_ uiOwnedMsg = accountLabelsLoadedMsg{}
	_ uiOwnedMsg = closeDialogMsg{}
	_ uiOwnedMsg = providerConfiguredResult{}
	_ uiOwnedMsg = modelSelectResult{}
	_ uiOwnedMsg = agentModelInitializedMsg{}
	_ uiOwnedMsg = modelSettingUpdatedMsg{}
	_ uiOwnedMsg = transparentToggledMsg{}
	_ uiOwnedMsg = themeSetMsg{}
	_ uiOwnedMsg = compactModeToggledMsg{}
	_ uiOwnedMsg = notificationStyleSetMsg{}
	_ uiOwnedMsg = permissionResponseMsg{}
	_ uiOwnedMsg = yoloToggledMsg{}
	_ uiOwnedMsg = notificationSentMsg{}
	_ uiOwnedMsg = importCopilotResult{}
	_ uiOwnedMsg = openEditorMsg{}
	_ uiOwnedMsg = shellStreamMsg{}
	_ uiOwnedMsg = agentModelChangedMsg{}
	_ uiOwnedMsg = copyChatHighlightMsg{}
	_ uiOwnedMsg = clearChatMouseMsg{}
	_ uiOwnedMsg = fileCompletionMsg{}
	_ uiOwnedMsg = pasteFilesCheckedMsg{}
	_ uiOwnedMsg = openEditorReadyMsg{}
	_ uiOwnedMsg = richPasteMsg{}
)
