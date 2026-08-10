package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newTestStrandCreate(t *testing.T) *StrandCreate {
	t.Helper()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	return NewStrandCreate(com)
}

// typeInto sends a KeyPressMsg for each rune of text into the dialog.
func typeInto(d *StrandCreate, text string) {
	for _, r := range text {
		d.HandleMsg(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func TestNormalizeStrandName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercases", "MyStrand", "mystrand"},
		{"spaces to dashes", "my strand name", "my-strand-name"},
		{"strips punctuation", "my!strand@name", "mystrandname"},
		{"collapses repeated dashes", "my--strand   name", "my-strand-name"},
		{"trims leading/trailing dashes", "-my-strand-", "my-strand"},
		{"all punctuation rejected", "!!!", ""},
		{"empty after trim", "   ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, normalizeStrandName(tc.in))
		})
	}
}

func TestStrandCreate_EmptyNameWarns(t *testing.T) {
	t.Parallel()

	d := newTestStrandCreate(t)
	// Move to the goal field and submit without ever typing a name.
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action, "advancing from name to goal should just move focus")

	action = d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "submitting with empty name should return ActionCmd carrying a warning, got %T", action)
	require.NotNil(t, cmdAction.Cmd)

	// A warning, not a submit, must not have advanced past the goal field.
	require.Equal(t, 1, d.focused)
}

func TestStrandCreate_PunctuationOnlyNameWarns(t *testing.T) {
	t.Parallel()

	d := newTestStrandCreate(t)
	typeInto(d, "!!!")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}) // advance to goal

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, ok := action.(ActionCmd)
	require.True(t, ok, "punctuation-only name should be rejected like an empty name, got %T", action)
}

func TestStrandCreate_ValidSubmitFromGoalField(t *testing.T) {
	t.Parallel()

	d := newTestStrandCreate(t)
	typeInto(d, "My Strand")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}) // advance to goal
	require.Equal(t, 1, d.focused)

	typeInto(d, "Fix the bug")
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	create, ok := action.(ActionCreateStrand)
	require.True(t, ok, "submitting from the goal field should return ActionCreateStrand, got %T", action)
	require.Equal(t, "my-strand", create.Name)
	require.Equal(t, "Fix the bug", create.Goal)
}

func TestStrandCreate_ValidSubmitWithEmptyGoal(t *testing.T) {
	t.Parallel()

	d := newTestStrandCreate(t)
	typeInto(d, "strand-x")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}) // advance to goal
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	create, ok := action.(ActionCreateStrand)
	require.True(t, ok, "empty goal must not block submission, got %T", action)
	require.Equal(t, "strand-x", create.Name)
	require.Equal(t, "", create.Goal)
}

func TestStrandCreate_TabMovesFocusWithoutSubmitting(t *testing.T) {
	t.Parallel()

	d := newTestStrandCreate(t)
	require.Equal(t, 0, d.focused)

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Nil(t, action, "tab should just move focus, not return an action")
	require.Equal(t, 1, d.focused)

	action = d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Nil(t, action)
	require.Equal(t, 0, d.focused, "tab from the last field should wrap around")

	action = d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.Nil(t, action)
	require.Equal(t, 1, d.focused, "shift+tab should move backward with wrap-around")
}

func TestStrandCreate_CloseKeyClosesDialog(t *testing.T) {
	t.Parallel()

	d := newTestStrandCreate(t)
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	_, ok := action.(ActionClose)
	require.True(t, ok, "esc should return ActionClose, got %T", action)
}

func TestStrandCreate_ID(t *testing.T) {
	t.Parallel()
	d := newTestStrandCreate(t)
	require.Equal(t, StrandCreateID, d.ID())
}
