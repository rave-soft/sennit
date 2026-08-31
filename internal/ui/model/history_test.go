package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

type historyWorkspace struct {
	workspace.Workspace
}

// KnownProviders: no test here renders a provider list.
func (w historyWorkspace) KnownProviders() []catwalk.Provider { return nil }

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w historyWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w historyWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w historyWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (historyWorkspace) Config() *config.Config {
	return &config.Config{}
}

func (historyWorkspace) PermissionSkipRequests() bool {
	return false
}

func (historyWorkspace) SupportsThreads() bool {
	return false
}

// SupportsTasks answers for the delegation list behind the panel's
// agents section; no test here drives one.
func (historyWorkspace) SupportsTasks() bool { return false }

// historyLoadRecordingWorkspace records which of ListUserMessages /
// ListAllUserMessages loadPromptHistory's returned tea.Cmd actually calls,
// and with what session ID, so a test can tell which session's history it
// loaded.
type historyLoadRecordingWorkspace struct {
	historyWorkspace
	calls []string
}

func (w *historyLoadRecordingWorkspace) ListUserMessages(_ context.Context, sessionID string) ([]message.Message, error) {
	w.calls = append(w.calls, "user:"+sessionID)
	return nil, nil
}

func (w *historyLoadRecordingWorkspace) ListAllUserMessages(context.Context) ([]message.Message, error) {
	w.calls = append(w.calls, "all")
	return nil, nil
}

func (w *historyLoadRecordingWorkspace) InitializePrompt() (string, error) {
	return "", nil
}

// TestLoadPromptHistorySnapshotsSessionAtCallTime covers the off-goroutine
// access bug: loadPromptHistory's returned tea.Cmd runs on the cmd
// goroutine, concurrently with further Update calls that can reassign
// m.sess.current. The cmd must decide which session to load from the
// state snapshotted when loadPromptHistory was called, not from m.sess
// read again when the cmd actually executes.
func TestLoadPromptHistorySnapshotsSessionAtCallTime(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	ws := &historyLoadRecordingWorkspace{}
	u.com.Workspace = ws
	u.sess.current = &session.Session{ID: "sess-a"}

	cmd := u.sess.loadPromptHistory(u.com)
	require.NotNil(t, cmd)

	// Simulate the session changing (e.g. the user switches sessions)
	// between loadPromptHistory building the cmd and Bubble Tea actually
	// running it.
	u.sess.current = nil

	cmd()

	require.Equal(t, []string{"user:sess-a"}, ws.calls,
		"cmd must load the session active when loadPromptHistory was called, not m.sess.current at execution time")
}

func TestHistoryBangCommandStripsPrefixWhileAlreadyInBangMode(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.com.Workspace = historyWorkspace{}
	u.editor.promptHistory.messages = []string{"!echo one", "!echo two"}
	u.editor.promptHistory.index = -1

	require.True(t, u.historyPrev())
	require.True(t, u.editor.bang.isActive())
	require.Equal(t, "echo one", u.editor.textarea.Value())

	require.True(t, u.historyPrev())
	require.True(t, u.editor.bang.isActive())
	require.Equal(t, "echo two", u.editor.textarea.Value())
}

// newEscTestUI builds a UI in uiChat/uiFocusEditor with the minimal wiring
// handleKeyPressMsg needs (keyMap, dialog overlay, attachments) to drive
// Escape through the real key-routing path rather than calling
// handleHistoryEscape directly.
func newEscTestUI(t *testing.T) *UI {
	t.Helper()
	u := newTestUI()
	u.com.Workspace = historyWorkspace{}
	u.keyMap = DefaultKeyMap()
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	return u
}

// TestDoubleEscClearsTypedDraft covers the "esc esc clears" invariant for
// plain typing (no history navigation involved): the first Esc has
// nothing to exit, so it leaves the draft untouched; the second
// consecutive Esc wipes it.
func TestDoubleEscClearsTypedDraft(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.textarea.InsertString("hello world")
	u.editor.promptHistory.index = -1

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, "hello world", u.editor.textarea.Value(), "first Esc must not clear the draft")

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Empty(t, u.editor.textarea.Value(), "second consecutive Esc must clear the draft")
}

// TestDoubleEscAfterHistoryNavClearsDraft covers the other half: when the
// first Esc has something to do (exit history navigation back to the
// draft), that's all it does — the second consecutive Esc is the one that
// clears the (now-restored) draft.
func TestDoubleEscAfterHistoryNavClearsDraft(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.textarea.InsertString("draft text")
	u.editor.promptHistory.messages = []string{"older message"}
	u.editor.promptHistory.index = -1

	require.True(t, u.historyPrev())
	require.Equal(t, "older message", u.editor.textarea.Value())

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, "draft text", u.editor.textarea.Value(), "first Esc must restore the draft")
	require.Equal(t, -1, u.editor.promptHistory.index)

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Empty(t, u.editor.textarea.Value(), "second consecutive Esc must clear the restored draft")
}

// TestEscThenTypingBreaksDoubleEscSequence: any key other than Escape
// between two Esc presses must reset the "last key was Esc" tracking, so
// a stray typed character doesn't turn a later, unrelated Esc into a
// clearing second-Esc.
func TestEscThenTypingBreaksDoubleEscSequence(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.textarea.InsertString("hello")
	u.editor.promptHistory.index = -1

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.True(t, u.editor.lastKeyWasEsc)

	u.handleKeyPressMsg(tea.KeyPressMsg{Text: "!"})
	require.False(t, u.editor.lastKeyWasEsc, "typing must break the Esc-Esc sequence")

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotEmpty(t, u.editor.textarea.Value(), "this Esc is the first of a new sequence, not a clearing second Esc")
}
