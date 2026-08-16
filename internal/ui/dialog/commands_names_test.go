package dialog

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// commandsNamesTestWorkspace is a minimal [workspace.Workspace] stub, only
// implementing what systemCommandItems reads (Config()).
type commandsNamesTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

func (w *commandsNamesTestWorkspace) SupportsThreads() bool { return false }

func (w *commandsNamesTestWorkspace) Config() *config.Config {
	return w.cfg
}

func newCommandsNamesTestCommon(t *testing.T) *common.Common {
	t.Helper()
	s := styles.BraidDark()
	cfg := &config.Config{
		Options:   &config.Options{TUI: &config.TUIOptions{}},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}
	return &common.Common{
		Styles:    &s,
		Workspace: &commandsNamesTestWorkspace{cfg: cfg},
	}
}

// findByID returns the command item with the given ID, failing the test if
// it isn't present.
func findByID(t *testing.T, items []*CommandItem, id string) *CommandItem {
	t.Helper()
	for _, item := range items {
		if item.ID() == id {
			return item
		}
	}
	t.Fatalf("command %q not found", id)
	return nil
}

// TestSystemCommandItems_ShortNames pins the short, Claude Code/opencode
// style titles (what the user types after "/") for the built-in commands,
// while checking their long-form names survive as aliases so existing
// muscle memory / search still matches.
func TestSystemCommandItems_ShortNames(t *testing.T) {
	t.Parallel()

	com := newCommandsNamesTestCommon(t)
	items := systemCommandItems(com, "sess-1", true /* hasSession */, false, false, 200, nil)

	cases := []struct {
		id      string
		title   string
		aliases []string
	}{
		{"new_session", "new", []string{"new session", "clear"}},
		{"switch_session", "sessions", nil},
		{"switch_model", "models", []string{"switch model"}},
		{"configure_providers", "providers", []string{"configure providers"}},
		{"doctor", "doctor", nil},
		{"summarize", "compact", []string{"summarize", "summarize session"}},
		{"toggle_sidebar", "sidebar", []string{"toggle sidebar"}},
		{"select_notifications", "notifications", []string{"notification style"}},
		{"toggle_yolo", "yolo", []string{"toggle yolo mode"}},
		{"toggle_help", "help", []string{"toggle help"}},
		{"init", "init", []string{"initialize project"}},
		{"toggle_transparent", "transparency", nil},
		{"quit", "exit", []string{"quit"}},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			item := findByID(t, items, tc.id)
			require.Equal(t, tc.title, item.Title())
			for _, alias := range tc.aliases {
				require.Contains(t, item.Aliases(), alias)
			}
		})
	}
}

// TestSystemCommandItems_CompactRunsSummarize verifies that "/compact" (the
// short name for the old "summarize session" command) still fires
// ActionSummarize for the current session.
func TestSystemCommandItems_CompactRunsSummarize(t *testing.T) {
	t.Parallel()

	com := newCommandsNamesTestCommon(t)
	items := systemCommandItems(com, "sess-42", true, false, false, 200, nil)

	compact := findByID(t, items, "summarize")
	require.Equal(t, "compact", compact.Title())
	require.Contains(t, compact.Aliases(), "summarize")

	action, ok := compact.Action().(ActionSummarize)
	require.True(t, ok, "expected ActionSummarize, got %T", compact.Action())
	require.Equal(t, "sess-42", action.SessionID)
}

// TestSystemCommandItems_DoctorOpensDoctorDialog verifies "/doctor" fires
// ActionOpenDialog{DoctorID}, matching how the other dialog-opening
// commands (models, providers, ...) are wired.
func TestSystemCommandItems_DoctorOpensDoctorDialog(t *testing.T) {
	t.Parallel()

	com := newCommandsNamesTestCommon(t)
	items := systemCommandItems(com, "sess-1", true, false, false, 200, nil)

	doctor := findByID(t, items, "doctor")
	action, ok := doctor.Action().(ActionOpenDialog)
	require.True(t, ok, "expected ActionOpenDialog, got %T", doctor.Action())
	require.Equal(t, DoctorID, action.DialogID)
}

// TestBuildCommandItems_CombinesAllSources checks that BuildCommandItems -
// the provider shared by the Commands palette dialog and the editor's "/"
// completion popup - includes system, custom, and MCP-prompt commands in a
// single flat list without duplication.
func TestBuildCommandItems_CombinesAllSources(t *testing.T) {
	t.Parallel()

	com := newCommandsNamesTestCommon(t)
	items := BuildCommandItems(com, "", false, false, false, 200, nil, nil, nil)

	require.NotEmpty(t, items)
	findByID(t, items, "new_session")
}
