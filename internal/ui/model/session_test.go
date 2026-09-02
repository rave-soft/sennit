package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestFileList(t *testing.T) {
	t.Parallel()

	t.Run("empty stats no truncation needed", func(t *testing.T) {
		t.Parallel()

		st := minimalFileStyles()
		files := []SessionFile{
			{FirstVersion: history.File{Path: "main.go"}, Additions: 0, Deletions: 0},
		}
		got := fileList(st, "/", files, 30, 10)
		require.Contains(t, stripANSI(got), "main.go")
	})

	t.Run("empty stats path truncates to width", func(t *testing.T) {
		t.Parallel()

		st := minimalFileStyles()
		files := []SessionFile{
			{FirstVersion: history.File{Path: "/very/long/path/to/some/deeply/nested/file.go"}, Additions: 0, Deletions: 0},
		}
		got := fileList(st, "/", files, 10, 10)
		plain := stripANSI(got)
		for _, line := range strings.Split(plain, "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), 10, "line exceeds sidebar width: %q", line)
		}
	})

	t.Run("with additions and deletions fits within width", func(t *testing.T) {
		t.Parallel()

		st := minimalFileStyles()
		files := []SessionFile{
			{FirstVersion: history.File{Path: "main.go"}, Additions: 5, Deletions: 3},
		}
		got := fileList(st, "/", files, 20, 10)
		plain := stripANSI(got)
		require.Contains(t, plain, "+5")
		require.Contains(t, plain, "-3")
		for _, line := range strings.Split(plain, "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), 20, "line exceeds sidebar width: %q", line)
		}
	})

	t.Run("narrow width with stats clamps path to zero", func(t *testing.T) {
		t.Parallel()

		st := minimalFileStyles()
		files := []SessionFile{
			{FirstVersion: history.File{Path: "main.go"}, Additions: 100, Deletions: 200},
		}
		got := fileList(st, "/", files, 5, 10)
		plain := stripANSI(got)
		require.NotContains(t, plain, "main.go")
		require.Equal(t, "+100 -200", strings.TrimSpace(plain))
	})

	t.Run("single addition only", func(t *testing.T) {
		t.Parallel()

		st := minimalFileStyles()
		files := []SessionFile{
			{FirstVersion: history.File{Path: "main.go"}, Additions: 3, Deletions: 0},
		}
		got := fileList(st, "/", files, 20, 10)
		plain := stripANSI(got)
		require.Contains(t, plain, "+3")
		require.NotContains(t, plain, "-0")
		for _, line := range strings.Split(plain, "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), 20, "line exceeds sidebar width: %q", line)
		}
	})

	t.Run("single deletion only", func(t *testing.T) {
		t.Parallel()

		st := minimalFileStyles()
		files := []SessionFile{
			{FirstVersion: history.File{Path: "main.go"}, Additions: 0, Deletions: 7},
		}
		got := fileList(st, "/", files, 20, 10)
		plain := stripANSI(got)
		require.NotContains(t, plain, "+0")
		require.Contains(t, plain, "-7")
		for _, line := range strings.Split(plain, "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), 20, "line exceeds sidebar width: %q", line)
		}
	})

	t.Run("max items zero returns empty", func(t *testing.T) {
		t.Parallel()

		st := minimalFileStyles()
		files := []SessionFile{
			{FirstVersion: history.File{Path: "main.go"}, Additions: 1, Deletions: 1},
		}
		got := fileList(st, "/", files, 20, 0)
		require.Empty(t, got)
	})
}

func TestModifiedFileAdditionsUseGreen(t *testing.T) {
	t.Parallel()

	st := styles.SennitDark()
	require.Equal(t, styles.BrandSuccess, st.Files.Additions.GetForeground())
}

func minimalFileStyles() *styles.Styles {
	st := styles.SennitDark()
	st.Files.Path = lipgloss.NewStyle()
	st.Files.Additions = lipgloss.NewStyle()
	st.Files.Deletions = lipgloss.NewStyle()
	st.Files.SectionTitle = lipgloss.NewStyle()
	st.Files.EmptyMessage = lipgloss.NewStyle()
	st.Files.TruncationHint = lipgloss.NewStyle()
	return &st
}

// fileHistoryWorkspace stubs the workspace and session-change role used by
// handleFileEvent's returned command.
type fileHistoryWorkspace struct {
	workspace.Workspace
}

// KnownProviders: no test here renders a provider list.
func (w fileHistoryWorkspace) KnownProviders() []catwalk.Provider { return nil }

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w fileHistoryWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w fileHistoryWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w fileHistoryWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (fileHistoryWorkspace) PrepareSessionChanges(context.Context, string) ([]workspace.SessionFile, error) {
	return nil, nil
}

func (fileHistoryWorkspace) Config() *config.Config { return nil }

// TestHandleFileEvent_SessionClearedBeforeCmdRuns is the regression case for
// the class of bug this package was audited for: a tea.Cmd closure must not
// read model state (here, m.sess.current) after Update returns, because a
// concurrent Update (e.g. ctrl+n's newSession) can mutate it first. The cmd
// must instead close over a captured session ID, like refreshModifiedFiles
// does, so clearing m.sess.current between the check and the cmd running
// does not panic.
func TestHandleFileEvent_SessionClearedBeforeCmdRuns(t *testing.T) {
	t.Parallel()

	com := common.DefaultCommon(context.Background(), fileHistoryWorkspace{})
	m := &UI{com: com}
	m.sess.current = &session.Session{ID: "s1"}

	cmd := m.sess.refreshModifiedFiles(m.com, m)
	require.NotNil(t, cmd)

	// Simulate ctrl+n clearing the session between Update returning the cmd
	// and the cmd goroutine actually running it.
	m.sess.current = nil

	require.NotPanics(t, func() {
		msg := cmd()
		_, ok := msg.(sessionFilesUpdatesMsg)
		require.True(t, ok, "expected a sessionFilesUpdatesMsg, got %T", msg)
	})
}

// TestFileEventFromAChildSessionRefreshes is the regression case for a
// subagent's work never reaching the sidebar: a delegated agent records its
// file versions under its own child session, so an event whose SessionID
// was compared for equality with the viewed session was dropped and the
// panel kept showing the parent's files alone until the parent's next tool
// call. The reload is scoped to the session tree, so the event only has to
// reach it.
func TestFileEventFromAChildSessionRefreshes(t *testing.T) {
	t.Parallel()

	com := common.DefaultCommon(context.Background(), fileHistoryWorkspace{})
	m := &UI{com: com}
	m.sess.current = &session.Session{ID: "s1"}

	cmds, _ := m.updateSession(pubsub.Event[history.File]{
		Payload: history.File{SessionID: "msg123$$call456", Path: "main.go"},
	}, nil)
	require.Len(t, cmds, 1, "a child session's file event must still refresh the list")

	msg, ok := cmds[0]().(sessionFilesUpdatesMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.sessionID, "the reload is scoped to the viewed session, not the event's")
}

// TestSameSessionFile pins what counts as "the same row" for the update
// this comparison gates: a reload that changed nothing must not bump the
// sidebar's version, and anything the panel or the diff view would render
// differently must.
func TestSameSessionFile(t *testing.T) {
	t.Parallel()

	base := SessionFile{
		FirstVersion:  history.File{ID: "v1", Path: "main.go"},
		LatestVersion: history.File{ID: "v2", Path: "main.go"},
		Additions:     3,
		Deletions:     1,
		Uncommitted:   true,
		GitKnown:      true,
	}
	require.True(t, sameSessionFile(base, base))

	newVersion := base
	newVersion.LatestVersion.ID = "v3"
	require.False(t, sameSessionFile(base, newVersion))

	committed := base
	committed.Uncommitted = false
	require.False(t, sameSessionFile(base, committed))

	restated := base
	restated.Additions = 4
	require.False(t, sameSessionFile(base, restated))
}

func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			esc = true
			continue
		}
		if esc {
			if s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' {
				esc = false
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestFileListFitsItsBudget pins the line count against maxItems. The
// truncation hint is a line of its own, and rendering it on top of a full
// maxItems rows made the section one line taller than the caller had
// allowed — enough to push the row below it off the panel.
func TestFileListFitsItsBudget(t *testing.T) {
	t.Parallel()

	sty := styles.Theme("")
	files := make([]SessionFile, 0, 10)
	for i := range 10 {
		files = append(files, SessionFile{
			FirstVersion: history.File{Path: fmt.Sprintf("/repo/file%d.go", i)},
			Additions:    1,
		})
	}

	for _, maxItems := range []int{1, 2, 5} {
		out := fileList(&sty, "/repo", files, 60, maxItems)
		lines := strings.Split(out, "\n")
		require.LessOrEqual(t, len(lines), maxItems,
			"maxItems=%d must bound the rendered lines, got:\n%s", maxItems, out)
		require.Contains(t, out, "more", "the hint must still be shown")
	}
}
