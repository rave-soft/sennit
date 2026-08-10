package dialog

import (
	"fmt"

	mcptools "github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/sahilm/fuzzy"
)

const (
	// DoctorID is the identifier for the config-problems dialog.
	DoctorID              = "doctor"
	doctorDialogMaxWidth  = 76
	doctorDialogMinHeight = 6
	doctorDialogMaxHeight = 20
)

// Doctor is a read-only dialog listing every config.Problem found in the
// current workspace: the /doctor command's UI, and the destination of the
// startup "N config problems" toast. It is a thin wrapper around
// [selectDialog] — there's nothing to act on beyond reading a problem's
// message and hint, so selecting an entry just closes the dialog.
type Doctor struct {
	*selectDialog
}

var _ Dialog = (*Doctor)(nil)

// NewDoctor creates the /doctor problems dialog.
func NewDoctor(com *common.Common) *Doctor {
	// buildItems never fails for the doctor list, so the error from
	// newSelectDialog is always nil here.
	sd, _ := newSelectDialog(com, selectDialogConfig{
		id:            DoctorID,
		title:         "Config Problems",
		maxWidth:      doctorDialogMaxWidth,
		dynamicHeight: true,
		minHeight:     doctorDialogMinHeight,
		maxHeight:     doctorDialogMaxHeight,
		buildItems:    func() ([]list.FilterableItem, int, error) { return doctorItems(com) },
		onSelect:      func(string) Action { return ActionClose{} },
	})
	return &Doctor{selectDialog: sd}
}

// DoctorProblems collects every config.Problem for this workspace: the
// static findings from config.Doctor plus any MCP server currently stuck
// in an error/needs-auth state. This mirrors braid_info's [problems]
// section (internal/agent/tools/braid_info.go's writeProblems) — the same
// merge, on the UI side of the workspace.Workspace boundary since
// internal/config cannot import the MCP client package.
func DoctorProblems(com *common.Common) []config.Problem {
	problems := config.Doctor(com.Config())
	for name, info := range com.Workspace.MCPGetStates() {
		if info.State != mcptools.StateError && info.State != mcptools.StateNeedsAuth {
			continue
		}
		msg := fmt.Sprintf("mcp server %s is in state %s", name, info.State)
		if info.Error != nil {
			msg += ": " + info.Error.Error()
		}
		problems = append(problems, config.Problem{
			Severity: config.SeverityError,
			Area:     config.AreaMCP,
			Subject:  name,
			Message:  msg,
		})
	}
	return problems
}

// doctorItems builds the dialog's list items in Problems' order (no
// meaningful default selection, so index 0).
func doctorItems(com *common.Common) ([]list.FilterableItem, int, error) {
	problems := DoctorProblems(com)
	items := make([]list.FilterableItem, 0, len(problems))
	for _, p := range problems {
		items = append(items, &DoctorItem{Versioned: list.NewVersioned(), problem: p, t: com.Styles})
	}
	return items, 0, nil
}

// DoctorItem renders one config.Problem as a list row.
type DoctorItem struct {
	*list.Versioned
	problem config.Problem
	t       *styles.Styles
	m       fuzzy.Match
	cache   map[int]string
	focused bool
}

var _ ListItem = (*DoctorItem)(nil)

// Finished implements list.Item. Doctor items are render-stable outside of
// explicit SetFocused / SetMatch.
func (d *DoctorItem) Finished() bool {
	return true
}

// Filter returns the value used for filtering.
func (d *DoctorItem) Filter() string {
	return string(d.problem.Area) + " " + d.problem.Subject + " " + d.problem.Message
}

// ID returns a unique identifier for the item, so repeated re-renders of
// the same problem list don't lose the selection.
func (d *DoctorItem) ID() string {
	return string(d.problem.Area) + ":" + d.problem.Subject + ":" + d.problem.Message
}

// SetFocused sets the focus state of the item.
func (d *DoctorItem) SetFocused(focused bool) {
	if d.focused == focused {
		return
	}
	d.cache = nil
	d.focused = focused
	if d.Versioned != nil {
		d.Bump()
	}
}

// SetMatch sets the fuzzy match for the item.
func (d *DoctorItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(d.m, m) {
		return
	}
	d.cache = nil
	d.m = m
	if d.Versioned != nil {
		d.Bump()
	}
}

// Render returns the string representation of the problem.
func (d *DoctorItem) Render(width int) string {
	st := ListItemStyles{
		ItemBlurred:     d.t.Dialog.NormalItem,
		ItemFocused:     d.t.Dialog.SelectedItem,
		InfoTextBlurred: d.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: d.t.Dialog.ListItem.InfoFocused,
	}
	title := fmt.Sprintf("[%s/%s] %s", d.problem.Severity, d.problem.Area, d.problem.Message)
	return renderItem(st, title, d.problem.Hint, d.focused, width, d.cache, &d.m)
}
