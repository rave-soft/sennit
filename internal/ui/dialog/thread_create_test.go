package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newTestThreadCreate(t *testing.T) *ThreadCreate {
	t.Helper()
	s := styles.BraidDark()
	com := &common.Common{Styles: &s}
	return NewThreadCreate(com)
}

// typeInto sends a KeyPressMsg for each rune of text into the dialog.
func typeInto(d *ThreadCreate, text string) {
	for _, r := range text {
		d.HandleMsg(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func TestNormalizeThreadName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercases", "MyThread", "mythread"},
		{"spaces to dashes", "my thread name", "my-thread-name"},
		{"strips punctuation", "my!thread@name", "mythreadname"},
		{"collapses repeated dashes", "my--thread   name", "my-thread-name"},
		{"trims leading/trailing dashes", "-my-thread-", "my-thread"},
		{"all punctuation rejected", "!!!", ""},
		{"empty after trim", "   ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, normalizeThreadName(tc.in))
		})
	}
}

func TestThreadCreate_EmptyNameWarns(t *testing.T) {
	t.Parallel()

	d := newTestThreadCreate(t)
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

func TestThreadCreate_PunctuationOnlyNameWarns(t *testing.T) {
	t.Parallel()

	d := newTestThreadCreate(t)
	typeInto(d, "!!!")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}) // advance to goal

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, ok := action.(ActionCmd)
	require.True(t, ok, "punctuation-only name should be rejected like an empty name, got %T", action)
}

func TestThreadCreate_ValidSubmitFromGoalField(t *testing.T) {
	t.Parallel()

	d := newTestThreadCreate(t)
	typeInto(d, "My Thread")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}) // advance to goal
	require.Equal(t, 1, d.focused)

	typeInto(d, "Fix the bug")
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	create, ok := action.(ActionCreateThread)
	require.True(t, ok, "submitting from the goal field should return ActionCreateThread, got %T", action)
	require.Equal(t, "my-thread", create.Name)
	require.Equal(t, "Fix the bug", create.Goal)
}

func TestThreadCreate_ValidSubmitWithEmptyGoal(t *testing.T) {
	t.Parallel()

	d := newTestThreadCreate(t)
	typeInto(d, "thread-x")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}) // advance to goal
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	create, ok := action.(ActionCreateThread)
	require.True(t, ok, "empty goal must not block submission, got %T", action)
	require.Equal(t, "thread-x", create.Name)
	require.Equal(t, "", create.Goal)
}

func TestThreadCreate_TabMovesFocusWithoutSubmitting(t *testing.T) {
	t.Parallel()

	d := newTestThreadCreate(t)
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

func TestThreadCreate_CloseKeyClosesDialog(t *testing.T) {
	t.Parallel()

	d := newTestThreadCreate(t)
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	_, ok := action.(ActionClose)
	require.True(t, ok, "esc should return ActionClose, got %T", action)
}

func TestThreadCreate_ID(t *testing.T) {
	t.Parallel()
	d := newTestThreadCreate(t)
	require.Equal(t, ThreadCreateID, d.ID())
}
