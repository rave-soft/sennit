package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// slashCommandsTestWorkspace is a minimal [workspace.Workspace] stub with
// just enough config (TUI options, an empty provider map) for
// systemCommandItems to build the command list without panicking on nil
// fields it doesn't defensively check.
type slashCommandsTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

// KnownProviders mirrors what the UI used to compute for itself:
// the embedded catalog for this fake's config.
func (w slashCommandsTestWorkspace) KnownProviders() []catwalk.Provider {
	return config.Providers(w.cfg)
}

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w slashCommandsTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w slashCommandsTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w slashCommandsTestWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (w *slashCommandsTestWorkspace) SupportsThreads() bool { return false }

// DockerMCPAvailable: unknown, so the palette offers no Docker entry and
// nothing runs a probe.
func (w *slashCommandsTestWorkspace) DockerMCPAvailable() (bool, bool) { return false, false }

// SupportsTasks answers for the delegation list behind the panel's
// agents section; no test here drives one.
func (w *slashCommandsTestWorkspace) SupportsTasks() bool { return false }

func (w *slashCommandsTestWorkspace) Config() *config.Config {
	return w.cfg
}

func (w *slashCommandsTestWorkspace) PermissionSkipRequests() bool {
	return false
}

// newSlashTestUI builds a uiChat/uiFocusEditor UI with the wiring
// handleKeyPressMsg needs to drive the "/" completion popup through the
// real key-routing path: a real KeyMap, dialog overlay, attachments, and a
// completions component (nil by default in newTestUI, since most layout
// tests never open it).
func newSlashTestUI(t *testing.T) *UI {
	t.Helper()
	u := newTestUI()
	u.com.Workspace = &slashCommandsTestWorkspace{cfg: &config.Config{
		Options:   &config.Options{TUI: &config.TUIOptions{}},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}}
	u.keyMap = DefaultKeyMap()
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	s := lipgloss.NewStyle()
	u.editor.completions = completions.New(completions.PopupStyles{
		Normal: s, Focused: s, Match: s, Muted: s, Border: s, ScrollbarThumb: s, ScrollbarTrack: s,
	})
	return u
}

func typeText(u *UI, text string) {
	for _, r := range text {
		u.handleKeyPressMsg(tea.KeyPressMsg{Text: string(r), Code: r})
	}
}

// TestSlashOnEmptyEditorOpensCommandCompletions covers the core UX change:
// "/" at the start of an empty editor now opens the inline completions
// popup (populated from the same command list as the Commands palette)
// instead of the old modal dialog.
func TestSlashOnEmptyEditorOpensCommandCompletions(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.handleKeyPressMsg(tea.KeyPressMsg{Text: "/", Code: '/'})

	require.True(t, u.editor.completionsOpen)
	require.Equal(t, completionsModeCommand, u.editor.completionsMode)
	require.True(t, u.editor.completions.IsOpen())
	require.True(t, u.editor.completions.HasItems())
	require.Equal(t, "/", u.editor.textarea.Value(), "the '/' itself is still typed into the editor")
	require.False(t, u.dialog.ContainsDialog(dialog.CommandsID), "the modal palette must not open")
}

// TestSlashMidTextIsPlainCharacter covers the opencode/Claude Code
// convention: "/" only triggers commands at the start of an empty editor.
// Typed after other text, it's just a character.
func TestSlashMidTextIsPlainCharacter(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "hello")
	u.handleKeyPressMsg(tea.KeyPressMsg{Text: "/", Code: '/'})

	require.False(t, u.editor.completionsOpen)
	require.Equal(t, "hello/", u.editor.textarea.Value())
}

// TestSlashFiltersAsUserTypes narrows the popup's item list to the query,
// the same fuzzy/substring matching used by @-file completions.
func TestSlashFiltersAsUserTypes(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "/mod")

	require.True(t, u.editor.completionsOpen)
	require.Equal(t, "mod", u.editor.completionsQuery)
	require.True(t, u.editor.completions.HasItems())
}

// TestSlashEnterRunsActionAndClearsEditor: Enter on a selected command runs
// its Action immediately (here, toggling the help view) and clears the
// draft, mirroring picking it from the Commands palette.
func TestSlashEnterRunsActionAndClearsEditor(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "/help")
	require.True(t, u.editor.completionsOpen)

	require.False(t, u.status.ShowingAll())
	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.True(t, u.status.ShowingAll(), "Enter must run the selected command's action")
	require.Empty(t, u.editor.textarea.Value(), "the editor is cleared after running a command")
	require.False(t, u.editor.completionsOpen)
}

// TestSlashTabInsertsNameWithoutRunning: Tab fills in the command's name
// (so custom/MCP commands that take arguments can be finished by hand)
// instead of running it.
func TestSlashTabInsertsNameWithoutRunning(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "/help")
	require.True(t, u.editor.completionsOpen)

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyTab, Text: "tab"})

	require.False(t, u.status.ShowingAll(), "Tab must not run the command")
	require.Equal(t, "/help ", u.editor.textarea.Value())
	require.False(t, u.editor.completionsOpen)
}

// TestSlashEscClosesPopupKeepsText: Esc exits command mode but leaves
// whatever was typed in the editor untouched.
func TestSlashEscClosesPopupKeepsText(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "/foo")
	require.True(t, u.editor.completionsOpen)

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})

	require.False(t, u.editor.completionsOpen)
	require.Equal(t, "/foo", u.editor.textarea.Value())
}

// TestSlashCompactRunsSummarize pins /compact as the short name for the old
// "summarize session" command.
func TestSlashCompactRunsSummarize(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.sess.current = &session.Session{ID: "sess-1"}
	items := u.commandCompletionItems()

	var found bool
	for _, item := range items {
		if item.ID == "summarize" {
			found = true
			require.Equal(t, "compact", item.Title)
			require.Contains(t, item.Aliases, "summarize")
			_, ok := item.Action.(dialog.ActionSummarize)
			require.True(t, ok, "expected ActionSummarize, got %T", item.Action)
		}
	}
	require.True(t, found, "expected a 'compact' command in the completion list")
}
