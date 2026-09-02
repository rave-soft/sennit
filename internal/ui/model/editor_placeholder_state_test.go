package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// newEditorPlaceholderStateWithValues is editorPlaceholderState's former
// production constructor for a fixed (non-randomized) placeholder pair;
// only tests ever needed a pinned value, so it lives here now (deadcode
// confirmed it unreachable from main).
func newEditorPlaceholderStateWithValues(ready, working string) editorPlaceholderState {
	return editorPlaceholderState{ready: ready, working: working}
}

func TestEditorPlaceholderStateSelectPlaceholder(t *testing.T) {
	state := newEditorPlaceholderStateWithValues("ready", "working")
	childPlaceholder := "viewing subagent session · ctrl+o to return"

	tests := []struct {
		name    string
		context editorPlaceholderContext
		want    string
	}{
		{name: "idle", want: "ready"},
		{name: "busy", context: editorPlaceholderContext{busy: true}, want: "working"},
		{name: "bang overrides busy", context: editorPlaceholderContext{bangActive: true, busy: true}, want: "Run a shell command"},
		{name: "child overrides bang", context: editorPlaceholderContext{viewingChildSession: true, exitChildSessionShortcut: "ctrl+o", bangActive: true}, want: childPlaceholder},
		{name: "yolo overrides child and busy", context: editorPlaceholderContext{viewingChildSession: true, exitChildSessionShortcut: "ctrl+o", busy: true, yolo: true}, want: "Yolo mode!"},
		{name: "bang prevents yolo override", context: editorPlaceholderContext{bangActive: true, yolo: true}, want: "Run a shell command"},
		{name: "unfocused preserves current", context: editorPlaceholderContext{current: "unchanged", busy: true}, want: "unchanged"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.context.editorFocused = test.name != "unfocused preserves current"
			require.Equal(t, test.want, state.selectPlaceholder(test.context))
		})
	}
}

func TestEditorPlaceholderStateRandomizeUsesKnownPools(t *testing.T) {
	state := newEditorPlaceholderState()
	require.Contains(t, readyPlaceholders, state.ready)
	require.Contains(t, workingPlaceholders, state.working)

	state.randomize()
	require.Contains(t, readyPlaceholders, state.ready)
	require.Contains(t, workingPlaceholders, state.working)
}

func TestEditorPlaceholderRandomizationTiming(t *testing.T) {
	m := newCmdDrivenGoldenUI(&cmdDrivingWorkspace{agentReady: true})
	m.editor.placeholder = newEditorPlaceholderStateWithValues("before-ready", "before-working")

	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, "before-ready", m.editor.placeholder.ready)
	require.Equal(t, "before-working", m.editor.placeholder.working)

	m.editor.textarea.SetValue("send this")
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Contains(t, readyPlaceholders, m.editor.placeholder.ready)
	require.Contains(t, workingPlaceholders, m.editor.placeholder.working)

	m.editor.placeholder = newEditorPlaceholderStateWithValues("before-ready", "before-working")
	m.editor.bang.enter(false)
	m.editor.textarea.SetValue("echo hello")
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Contains(t, readyPlaceholders, m.editor.placeholder.ready)
	require.Contains(t, workingPlaceholders, m.editor.placeholder.working)
}
