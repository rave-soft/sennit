package model

// Root is the top-level Bubble Tea model (see internal/cmd/root.go). It
// routes between three screens — the main session UI, the strands
// dashboard, and an attached strand's own embedded UI — and owns the pieces
// that don't belong to any single screen: the strand cache/dashboard is
// lazily created on first use, and an attached strand's workspace/event
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
	screenStrand
)

// strandEventSubscriber is implemented by the concrete workspace types
// returned from AttachStrand (see internal/workspace/strands.go). It is not
// part of workspace.Workspace itself — SubscribeWith is a second,
// independently stoppable subscription, distinct from the primary
// ws.Subscribe(program) pump cmd/root.go starts for the main workspace —
// so a strand's own event stream can be torn down on detach without
// disturbing the main one.
type strandEventSubscriber interface {
	SubscribeWith(send func(tea.Msg)) func()
}

// strandAttachment holds everything tied to the strand currently attached
// for viewing: its own workspace, an embedded UI over that workspace, and
// the teardown funcs for both the event pump and the workspace connection
// itself.
type strandAttachment struct {
	strandID string
	ws       workspace.Workspace
	ui       *UI
	stop     func() // stops the SubscribeWith event pump
	detach   func() // releases the attached workspace (from AttachStrand)
}

// Root is the top-level tea.Model. See the package doc comment above.
type Root struct {
	com             *common.Common
	main            *UI
	dashboard       *strandsDashboard // lazily created on first ctrl+e
	dashboardDialog *dialog.Overlay   // hosts the strand-create dialog while on the dashboard screen
	strand          *strandAttachment // non-nil while attached to a strand
	active          screenID

	// send delivers messages back into the Bubble Tea event loop from
	// outside Update (the per-strand SubscribeWith pump runs its own
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

// SetSend wires the tea.Program's Send method so the strand event pump
// (started outside Update) can deliver messages back into the loop.
func (r *Root) SetSend(send func(tea.Msg)) { r.send = send }

// strandEventMsg tags a message from an attached strand's own event pump
// with the strand ID it came from, so a detach racing the pump's goroutine
// can be dropped instead of misrouted to whatever strand is attached next.
type strandEventMsg struct {
	strandID string
	inner    tea.Msg
}

// strandAttachedMsg delivers the result of an off-thread AttachStrand call.
type strandAttachedMsg struct {
	id        string
	sessionID string
	ws        workspace.Workspace
	detach    func()
	err       error
}

// strandActionDoneMsg delivers the result of an off-thread merge/remove
// call; both actions only need pass/fail plus a dashboard refresh.
type strandActionDoneMsg struct {
	err error
}

// strandCreatedMsg delivers the result of an off-thread CreateStrand call.
type strandCreatedMsg struct {
	strand proto.Strand
	err    error
}

// Init implements tea.Model.
func (r *Root) Init() tea.Cmd {
	return r.main.Init()
}

// View implements tea.Model.
func (r *Root) View() tea.View {
	switch r.active {
	case screenStrand:
		return r.strand.ui.View()
	case screenDashboard:
		return r.dashboardView()
	default:
		return r.main.View()
	}
}

// dashboardView builds the strands dashboard's screen from scratch — unlike
// screenMain/screenStrand, it has no *UI of its own to delegate to. String
// hygiene (newline normalization, trailing-space trim) mirrors UI.View.
func (r *Root) dashboardView() tea.View {
	var v tea.View
	v.AltScreen = true
	v.BackgroundColor = r.com.Styles.Background
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "braid strands"

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
	case strandEventMsg:
		// A message from an attached strand's own event pump. Racing a
		// detach is expected (the pump's goroutine can't be joined
		// synchronously), so a mismatch is silently dropped, not an error.
		if r.strand != nil && r.strand.strandID == msg.strandID {
			_, cmd := r.strand.ui.Update(msg.inner)
			return r, cmd
		}
		return r, nil
	case pubsub.Event[proto.Strand]:
		var cmds []tea.Cmd
		_, cmd := r.main.Update(msg)
		cmds = append(cmds, cmd)
		if r.dashboard != nil {
			cmds = append(cmds, r.dashboard.ApplyStrandEvent(msg))
		}
		return r, tea.Batch(cmds...)
	case tea.KeyPressMsg:
		return r.handleKeyPress(msg)
	case showStrandsDashboardMsg:
		if r.dashboard == nil {
			r.dashboard = newStrandsDashboard(r.com)
			r.dashboard.SetSize(r.width, r.height)
		}
		cmd := r.dashboard.SetActive(true)
		r.active = screenDashboard
		return r, cmd
	case strandsLoadedMsg:
		if r.dashboard == nil {
			return r, nil
		}
		return r, tea.Batch(r.dashboard.ApplyStrandsLoaded(msg)...)
	case enterStrandMsg:
		return r, r.attachStrandCmd(msg.id, msg.sessionID)
	case strandAttachedMsg:
		return r.handleStrandAttached(msg)
	case mergeStrandMsg:
		return r, r.mergeStrandCmd(msg.id)
	case removeStrandMsg:
		return r, r.removeStrandCmd(msg.id)
	case strandActionDoneMsg:
		if msg.err != nil {
			return r, util.ReportError(msg.err)
		}
		if r.dashboard != nil {
			return r, r.dashboard.Refresh()
		}
		return r, nil
	case strandCreatedMsg:
		if msg.err != nil {
			return r, util.ReportError(msg.err)
		}
		if r.dashboard != nil {
			return r, r.dashboard.ApplyStrandEvent(pubsub.Event[proto.Strand]{
				Type:    pubsub.CreatedEvent,
				Payload: msg.strand,
			})
		}
		return r, nil
	case util.InfoMsg:
		// Errors from strand actions (attach/merge/remove/create) surface
		// here. Known limitation: the dashboard and strand screens have no
		// status line of their own yet, so these only become visible once
		// the user returns to the main screen.
		_, cmd := r.main.Update(msg)
		return r, cmd
	default:
		switch r.active {
		case screenStrand:
			if r.strand != nil {
				_, cmd := r.strand.ui.Update(msg)
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
	if r.strand != nil {
		_, cmd := r.strand.ui.Update(msg)
		cmds = append(cmds, cmd)
	}
	return r, tea.Batch(cmds...)
}

// handleKeyPress routes a key press according to which screen is active.
// "Go back" keys are checked first so they always win over screen-local
// bindings, per AGENTS.md's "dialog messages are intercepted first" —
// applied here one level up, to screen navigation.
func (r *Root) handleKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	toggle := key.Matches(msg, r.main.KeyMap().Strands)

	if r.active != screenMain {
		switch {
		case toggle && r.active == screenDashboard:
			r.active = screenMain
			r.dashboard.SetActive(false)
			return r, nil
		case toggle && r.active == screenStrand:
			return r, r.leaveStrand()
		case key.Matches(msg, dialog.CloseKey) && r.active == screenDashboard && !r.dashboardDialog.HasDialogs():
			r.active = screenMain
			r.dashboard.SetActive(false)
			return r, nil
		}
	}

	switch r.active {
	case screenDashboard:
		return r.handleDashboardKey(msg)
	case screenStrand:
		_, cmd := r.strand.ui.Update(msg)
		return r, cmd
	default:
		_, cmd := r.main.Update(msg)
		return r, cmd
	}
}

// handleDashboardKey routes a key press while the dashboard screen is
// active: the create-strand dialog wins when open, otherwise the dashboard
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
// for the actions the strand-create dialog can produce.
func (r *Root) handleDashboardDialogAction(action dialog.Action) tea.Cmd {
	switch action := action.(type) {
	case dialog.ActionClose:
		r.dashboardDialog.CloseFrontDialog()
	case dialog.ActionCmd:
		return action.Cmd
	case dialog.ActionCreateStrand:
		r.dashboardDialog.CloseFrontDialog()
		return r.createStrandCmd(action.Name, action.Goal)
	}
	return nil
}

// attachStrandCmd calls AttachStrand off-thread. Per AGENTS.md, model state
// is never touched inside a command closure — only locals are captured.
func (r *Root) attachStrandCmd(id, sessionID string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		attached, detach, err := ws.AttachStrand(ctx, id)
		return strandAttachedMsg{id: id, sessionID: sessionID, ws: attached, detach: detach, err: err}
	}
}

// handleStrandAttached applies the result of attachStrandCmd. On success it
// builds the strand's own embedded UI, starts its event pump, and switches
// the active screen; on failure it reports the error once the user is back
// on the main screen (see the util.InfoMsg case in Update).
func (r *Root) handleStrandAttached(msg strandAttachedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return r, util.ReportError(msg.err)
	}

	com := common.DefaultCommon(r.com.Context(), msg.ws)
	childUI := New(com, msg.sessionID, false, WithEmbedded())

	stop := func() {}
	if sub, ok := msg.ws.(strandEventSubscriber); ok {
		id := msg.id
		stop = sub.SubscribeWith(func(m tea.Msg) {
			if r.send != nil {
				r.send(strandEventMsg{strandID: id, inner: m})
			}
		})
	}

	r.strand = &strandAttachment{
		strandID: msg.id,
		ws:       msg.ws,
		ui:       childUI,
		stop:     stop,
		detach:   msg.detach,
	}
	r.active = screenStrand

	var cmds []tea.Cmd
	cmds = append(cmds, childUI.Init())
	_, cmd := childUI.Update(tea.WindowSizeMsg{Width: r.width, Height: r.height})
	cmds = append(cmds, cmd)
	return r, tea.Batch(cmds...)
}

// leaveStrand tears down the attached strand and returns to the dashboard.
// detach may do IO (e.g. shutting down a client connection), so it runs
// inside the returned tea.Cmd rather than synchronously here.
func (r *Root) leaveStrand() tea.Cmd {
	if r.strand == nil {
		r.active = screenDashboard
		return nil
	}
	strand := r.strand
	if strand.stop != nil {
		strand.stop()
	}
	r.strand = nil
	r.active = screenDashboard

	var cmds []tea.Cmd
	if strand.detach != nil {
		cmds = append(cmds, func() tea.Msg {
			strand.detach()
			return nil
		})
	}
	if r.dashboard != nil {
		// The strand may have changed status while attached (e.g.
		// completed); invalidate so the dashboard doesn't sit on a stale
		// row until the TTL backstop catches up.
		r.dashboard.cache.invalidateStrands()
		cmds = append(cmds, r.dashboard.Refresh())
	}
	return tea.Batch(cmds...)
}

// mergeStrandCmd calls MergeStrand off-thread.
func (r *Root) mergeStrandCmd(id string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		_, err := ws.MergeStrand(ctx, id)
		return strandActionDoneMsg{err: err}
	}
}

// removeStrandCmd calls RemoveStrand off-thread.
func (r *Root) removeStrandCmd(id string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		err := ws.RemoveStrand(ctx, id, proto.RemoveStrandOptions{})
		return strandActionDoneMsg{err: err}
	}
}

// createStrandCmd calls CreateStrand off-thread with the dialog's validated
// input.
func (r *Root) createStrandCmd(name, goal string) tea.Cmd {
	ctx := r.com.Context()
	ws := r.com.Workspace
	return func() tea.Msg {
		strand, err := ws.CreateStrand(ctx, proto.CreateStrandRequest{Name: name, Goal: goal})
		return strandCreatedMsg{strand: strand, err: err}
	}
}

// Cleanup releases the attached strand's resources, if any. Called once by
// cmd/root.go after program.Run() returns — best-effort, since there is no
// further chance to surface an error through the (now-stopped) TUI.
func (r *Root) Cleanup() {
	if r.strand == nil {
		return
	}
	if r.strand.stop != nil {
		r.strand.stop()
	}
	if r.strand.detach != nil {
		r.strand.detach()
	}
	r.strand = nil
}
