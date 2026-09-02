package dialog

import (
	"fmt"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

const (
	// DoctorID is the identifier for the config-problems dialog.
	DoctorID              = "doctor"
	doctorDialogMaxWidth  = 76
	doctorDialogMinHeight = 6
	doctorDialogMaxHeight = 20
)

// doctorMode selects which of the dialog's two screens HandleMsg/Draw are
// currently showing: the filterable problem list, or one problem's full
// detail (message + hint, untruncated, plus an area-specific fix action).
type doctorMode int

const (
	doctorModeList doctorMode = iota
	doctorModeDetail
)

// doctorShowDetail is returned by the list's onSelect to ask Doctor.HandleMsg
// to switch to the detail screen instead of closing the dialog. It never
// escapes the dialog package — Action is `any`, so this is a private
// implementation detail of the list->detail handoff.
type doctorShowDetail struct{ id string }

// doctorProblemsLoadedMsg carries the result of com.Workspace.DoctorProblems,
// run off the Update goroutine — see loadDoctorProblemsCmd.
type doctorProblemsLoadedMsg struct{ problems []config.Problem }

// Doctor is the /doctor command's dialog: a filterable list of every
// config.Problem in the workspace, backed by [selectDialog]. Enter on a
// problem switches to a detail screen with the untruncated message/hint
// and, for provider/model problems, a key to jump straight to the dialog
// that fixes it.
//
// The problem list is not known at construction: NewDoctor returns before
// it has one (see doctorProblemsLoadedMsg), so the dialog opens empty and
// populates itself once the workspace answers.
type Doctor struct {
	*selectDialog
	Base
	com      *common.Common
	mode     doctorMode
	problems []config.Problem
	detail   config.Problem

	help   help.Model
	keyMap struct {
		Back          key.Binding
		OpenProviders key.Binding
		OpenModels    key.Binding
		Close         key.Binding
	}
}

var _ Dialog = (*Doctor)(nil)

// NewDoctor creates the /doctor problems dialog. It performs no IO itself:
// the returned dialog starts with an empty problem list, and the returned
// tea.Cmd fetches the real one from com.Workspace.DoctorProblems — which
// shells out to probe the environment (missing clipboard helpers, etc.) and
// so must not run on the Update goroutine. See NewFilePicker for the same
// (dialog, tea.Cmd) shape.
func NewDoctor(com *common.Common) (*Doctor, tea.Cmd) {
	d := &Doctor{Base: NewBase(com, doctorDialogMaxWidth), com: com}

	// buildItems never fails for the doctor list, so the error from
	// newSelectDialog is always nil here.
	sd, _ := newSelectDialog(com, selectDialogConfig{
		id:            DoctorID,
		title:         "Config Problems",
		maxWidth:      doctorDialogMaxWidth,
		dynamicHeight: true,
		minHeight:     doctorDialogMinHeight,
		maxHeight:     doctorDialogMaxHeight,
		buildItems:    func() ([]list.FilterableItem, int, error) { return doctorItemsFrom(com, d.problems) },
		onSelect:      func(id string) Action { return doctorShowDetail{id: id} },
	})
	d.selectDialog = sd

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	d.help = h

	d.keyMap.Back = key.NewBinding(
		key.WithKeys("esc", "alt+esc"),
		key.WithHelp("esc", "back"),
	)
	d.keyMap.OpenProviders = key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "open providers"),
	)
	d.keyMap.OpenModels = key.NewBinding(
		key.WithKeys("m"),
		key.WithHelp("m", "open models"),
	)
	d.keyMap.Close = CloseKey

	return d, loadDoctorProblemsCmd(com)
}

// loadDoctorProblemsCmd fetches the merged problem list off the Update
// goroutine (see Workspace.DoctorProblems's doc comment for why it can
// block) and reports it as doctorProblemsLoadedMsg.
func loadDoctorProblemsCmd(com *common.Common) tea.Cmd {
	ws := com.Workspace
	return func() tea.Msg {
		return doctorProblemsLoadedMsg{problems: ws.DoctorProblems()}
	}
}

// HandleMsg implements [Dialog], overriding the embedded selectDialog to
// add the list<->detail state machine. The list screen still gets its key
// handling (filter, up/down, esc-to-close) straight from selectDialog; only
// the Enter/Select outcome is intercepted, since onSelect above turns it
// into a doctorShowDetail instead of an ActionClose.
func (d *Doctor) HandleMsg(msg tea.Msg) Action {
	if loaded, ok := msg.(doctorProblemsLoadedMsg); ok {
		d.problems = loaded.problems
		// The item source changed out from under the user (they didn't
		// ask for this reload — it's the initial load landing), so keep
		// whatever filter they've already typed.
		_ = d.replaceItems(func() ([]list.FilterableItem, int, error) { return doctorItemsFrom(d.com, d.problems) }, true)
		return nil
	}

	if d.mode == doctorModeDetail {
		return d.handleDetailMsg(msg)
	}

	action := d.selectDialog.HandleMsg(msg)
	if show, ok := action.(doctorShowDetail); ok {
		for _, p := range d.problems {
			if doctorItemID(p) == show.id {
				d.detail = p
				d.mode = doctorModeDetail
				break
			}
		}
		return nil
	}
	return action
}

// handleDetailMsg handles keys on the detail screen: esc goes back to the
// list (it does not close the dialog — that's what makes it a "back", not
// a "close"), and p/m are area-specific fix shortcuts that close Doctor and
// open the dialog that actually fixes the problem (provider/model config
// lives outside a diagnostic dialog's remit). Agent problems have no fix
// shortcut: the fix is editing an agent definition file, which isn't
// something any dialog does today.
func (d *Doctor) handleDetailMsg(msg tea.Msg) Action {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch {
	case key.Matches(keyMsg, d.keyMap.Back):
		d.mode = doctorModeList
		return nil
	case d.detail.Area == config.AreaProvider && key.Matches(keyMsg, d.keyMap.OpenProviders):
		return ActionOpenDialog{DialogID: ProvidersID}
	case d.detail.Area == config.AreaModel && key.Matches(keyMsg, d.keyMap.OpenModels):
		return ActionOpenDialog{DialogID: ModelsID}
	}
	return nil
}

// Draw implements [Dialog].
func (d *Doctor) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	if d.mode == doctorModeList {
		return d.selectDialog.Draw(scr, area)
	}
	return d.drawDetail(scr, area)
}

// drawDetail renders the full text of the currently selected problem: area,
// severity, subject, the untruncated message and hint (the list truncates
// long ones to fit a row), and a help footer with whatever fix shortcut
// applies.
func (d *Doctor) drawDetail(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	d.Resize(area)
	width := d.Width()
	innerWidth := max(0, d.InnerWidth())

	wrap := lipgloss.NewStyle().Width(innerWidth)
	labelStyle := t.Dialog.ListItem.InfoBlurred
	var lines []string
	lines = append(lines, wrap.Render(labelStyle.Render("area: ")+string(d.detail.Area)+
		labelStyle.Render("  severity: ")+string(d.detail.Severity)))
	if d.detail.Subject != "" {
		lines = append(lines, wrap.Render(labelStyle.Render("subject: ")+d.detail.Subject))
	}
	lines = append(lines, "", wrap.Render(d.detail.Message))
	if d.detail.Hint != "" {
		lines = append(lines, "", wrap.Render(labelStyle.Render("hint: ")+d.detail.Hint))
	}
	content := lipgloss.JoinVertical(lipgloss.Left, lines...)

	rc := NewRenderContext(t, width)
	rc.Title = "Problem Details"
	rc.AddPart(content)
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)

	view := rc.Render()
	DrawCenter(scr, area, view)
	return nil
}

// ShortHelp implements [help.KeyMap]. In list mode this delegates to
// selectDialog's own bindings; in detail mode it shows back plus whatever
// fix shortcut applies to the current problem's area.
func (d *Doctor) ShortHelp() []key.Binding {
	if d.mode == doctorModeList {
		return d.selectDialog.ShortHelp()
	}
	bindings := []key.Binding{d.keyMap.Back}
	switch d.detail.Area {
	case config.AreaProvider:
		bindings = append(bindings, d.keyMap.OpenProviders)
	case config.AreaModel:
		bindings = append(bindings, d.keyMap.OpenModels)
	}
	return bindings
}

// FullHelp implements [help.KeyMap].
func (d *Doctor) FullHelp() [][]key.Binding {
	if d.mode == doctorModeList {
		return d.selectDialog.FullHelp()
	}
	return [][]key.Binding{d.ShortHelp()}
}

// doctorItemID builds the stable id shared by DoctorItem.ID() (what the
// list reports back through onSelect) and Doctor.HandleMsg's lookup back
// to the full config.Problem.
func doctorItemID(p config.Problem) string {
	return string(p.Area) + ":" + p.Subject + ":" + p.Message
}

// doctorItemsFrom builds the dialog's list items from an already-collected
// problem set (shared with Doctor.problems so both see the same snapshot).
func doctorItemsFrom(com *common.Common, problems []config.Problem) ([]list.FilterableItem, int, error) {
	items := make([]list.FilterableItem, 0, len(problems))
	for _, p := range problems {
		items = append(items, &DoctorItem{BaseItem: list.NewBaseItem(), problem: p, t: com.Styles})
	}
	return items, 0, nil
}

// DoctorItem renders one config.Problem as a list row.
type DoctorItem struct {
	list.BaseItem
	problem config.Problem
	t       *styles.Styles
}

var _ ListItem = (*DoctorItem)(nil)

// Filter returns the value used for filtering.
func (d *DoctorItem) Filter() string {
	return string(d.problem.Area) + " " + d.problem.Subject + " " + d.problem.Message
}

// ID returns a unique identifier for the item, so repeated re-renders of
// the same problem list don't lose the selection, and so Doctor.HandleMsg
// can look the full config.Problem back up when this item is selected.
func (d *DoctorItem) ID() string {
	return doctorItemID(d.problem)
}

// Render returns the string representation of the problem. The message and
// hint are shown truncated to fit the row; Enter opens the detail screen
// for the untruncated text.
func (d *DoctorItem) Render(width int) string {
	st := defaultListItemStyles(d.t)
	title := fmt.Sprintf("[%s/%s] %s", d.problem.Severity, d.problem.Area, d.problem.Message)
	return renderItem(st, title, d.problem.Hint, d.Focused(), width, d.Cache(), d.Match())
}
