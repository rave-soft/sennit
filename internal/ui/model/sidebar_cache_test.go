package model

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// sidebarCacheWorkspace is a minimal workspace.Workspace stub for the
// sidebar cache tests: just Config, WorkingDir, and a mutable
// BackgroundJobCounts (the mutex-guarded call the cache exists to avoid
// paying for on every frame). The embedded interface panics on anything
// else, which is the point: a full sidebar render with an empty session
// never needs more than this.
type sidebarCacheWorkspace struct {
	workspace.Workspace

	cfg     *config.Config
	cwd     string
	counts  workspace.BackgroundJobCounts
	countsN int // number of BackgroundJobCounts calls, for tests that want to distinguish
}

func (w *sidebarCacheWorkspace) Config() *config.Config         { return w.cfg }
func (w *sidebarCacheWorkspace) WorkingDir() string             { return w.cwd }
func (w *sidebarCacheWorkspace) BuiltinSkills() []*skills.Skill { return nil }
func (w *sidebarCacheWorkspace) BackgroundJobCounts() workspace.BackgroundJobCounts {
	w.countsN++
	return w.counts
}

// newSidebarCacheTestUI builds a *UI in uiChat, non-compact, with an active
// session and a sidebar area wide/tall enough to render every section —
// everything updateSidebarScrollState needs.
func newSidebarCacheTestUI(t *testing.T) (*UI, *sidebarCacheWorkspace) {
	t.Helper()

	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Agents:    map[string]config.Agent{},
		Options:   &config.Options{TUI: &config.TUIOptions{}},
	}
	ws := &sidebarCacheWorkspace{cfg: cfg, cwd: "/tmp/project"}
	com := common.DefaultCommon(context.Background(), ws)

	ta := textarea.New()
	ta.SetStyles(com.Styles.Editor.Textarea)
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	ta.SetVirtualCursor(false)
	ta.DynamicHeight = true
	ta.MinHeight = TextareaMinHeight
	ta.MaxHeight = TextareaMaxHeight
	ta.Focus()

	u := &UI{
		com: com,
		widgets: widgets{
			status: NewStatus(com, nil),
			chat:   chatlist.NewChat(com, config.ScrollbarDefault),
			dialog: dialog.NewOverlay(),
			header: newHeader(com),
		},
		editor: editorState{
			textarea:    ta,
			attachments: attachments.New(nil, attachments.Keymap{}),
		},
		state:  uiChat,
		focus:  uiFocusEditor,
		keyMap: DefaultKeyMap(),
		lay: layoutState{
			width:  140,
			height: 45,
		},
	}
	u.status.helpKm = u
	u.sess.current = &session.Session{ID: "s1", Title: "Test Session"}
	u.updateLayoutAndSize()
	require.False(t, u.lay.isCompact, "sidebar cache tests need the non-compact layout")
	require.False(t, u.lay.layout.sidebar.Dx() == 0, "sidebar area must be non-empty for these tests")
	return u, ws
}

// TestUpdateSidebarScrollState_SkipsRenderWhenNothingChanged proves the
// cache hit path: with every tracked input unchanged, a second call must
// not touch m.sidebar.content at all. Poisoning content before the second
// call and asserting it survives is the only way to observe "the render
// didn't run" from outside — see chat_draw_cache_test.go for the same
// pattern applied to Chat's draw cache.
func TestUpdateSidebarScrollState_SkipsRenderWhenNothingChanged(t *testing.T) {
	t.Parallel()

	u, ws := newSidebarCacheTestUI(t)
	u.updateSidebarScrollState()
	require.NotEmpty(t, u.sidebar.content)
	callsAfterFirst := ws.countsN

	const poison = "\x00poison\x00"
	u.sidebar.content = poison

	u.updateSidebarScrollState()
	require.Equal(t, poison, u.sidebar.content,
		"an unchanged signature must skip the render entirely, leaving content untouched")
	require.Greater(t, ws.countsN, callsAfterFirst,
		"the signature is still recomputed every frame (cheap); only the render is skipped")
}

// TestUpdateSidebarScrollState_RerendersWhenSessionChanges covers the most
// common invalidation: switching to a different session (a fresh *Session
// pointer, per sessionState's doc comment) must force a re-render even
// though every other input is untouched.
func TestUpdateSidebarScrollState_RerendersWhenSessionChanges(t *testing.T) {
	t.Parallel()

	u, _ := newSidebarCacheTestUI(t)
	u.updateSidebarScrollState()

	const poison = "\x00poison\x00"
	u.sidebar.content = poison
	u.sess.current = &session.Session{ID: "s1", Title: "Renamed"}

	u.updateSidebarScrollState()
	require.NotEqual(t, poison, u.sidebar.content, "a session swap must invalidate the cache")
	require.Contains(t, u.sidebar.content, "Renamed")
}

// TestUpdateSidebarScrollState_RerendersOnJobCountsChange covers the
// specific cost flagged in review: a background job starting/finishing
// changes BackgroundJobCounts without touching the session, LSP, MCP, or
// skill state, and the sidebar must still pick it up.
func TestUpdateSidebarScrollState_RerendersOnJobCountsChange(t *testing.T) {
	t.Parallel()

	u, ws := newSidebarCacheTestUI(t)
	u.updateSidebarScrollState()

	const poison = "\x00poison\x00"
	u.sidebar.content = poison
	ws.counts = workspace.BackgroundJobCounts{Active: 2}

	u.updateSidebarScrollState()
	require.NotEqual(t, poison, u.sidebar.content, "a job-count change must invalidate the cache")
	require.Contains(t, ansi.Strip(u.sidebar.content), "Active 2/50")
}

// TestUpdateSidebarScrollState_RerendersOnFilesVersionBump covers the
// version-counter invalidation path for m.sess.files: sessionFilesUpdatesMsg
// replaces the slice wholesale (see update_session.go), so the cache keys
// off sessionState.filesVersion rather than diffing the slice.
func TestUpdateSidebarScrollState_RerendersOnFilesVersionBump(t *testing.T) {
	t.Parallel()

	u, _ := newSidebarCacheTestUI(t)
	u.updateSidebarScrollState()

	const poison = "\x00poison\x00"
	u.sidebar.content = poison
	u.sess.files = []SessionFile{{Additions: 3}}
	u.sess.filesVersion++

	u.updateSidebarScrollState()
	require.NotEqual(t, poison, u.sidebar.content, "a files-version bump must invalidate the cache")
}

// TestUpdateSidebarScrollState_ClampsOffsetOnCacheHit covers the one thing
// that must still happen on a cache hit: an out-of-range scroll offset
// (e.g. left over from a taller previous render) gets clamped against the
// cached maxOffset even though the content itself is reused.
func TestUpdateSidebarScrollState_ClampsOffsetOnCacheHit(t *testing.T) {
	t.Parallel()

	u, _ := newSidebarCacheTestUI(t)
	u.updateSidebarScrollState()

	u.sidebar.offset = u.sidebar.maxOffset + 50
	u.updateSidebarScrollState()
	require.Equal(t, u.sidebar.maxOffset, u.sidebar.offset)
}

// TestComputeSidebarSig_StableAcrossIdenticalCalls guards the signature
// type itself: two computations back to back, with nothing mutated in
// between, must compare equal. sidebarSig is used as a plain `==` map/struct
// key in updateSidebarScrollState, so any field that isn't consistently
// comparable (e.g. reading a live map's length differently across calls)
// would silently defeat the cache.
func TestComputeSidebarSig_StableAcrossIdenticalCalls(t *testing.T) {
	t.Parallel()

	u, _ := newSidebarCacheTestUI(t)
	a := u.computeSidebarSig()
	b := u.computeSidebarSig()
	require.Equal(t, a, b)
}

// TestSidebarCacheInvalidatesOnThemeChange pins the one input that is not
// a field of the model: the palette. setTheme swaps *com.Styles in place,
// so the pointer the signature could compare is unchanged, and every other
// keyed input — the area, the session, the version counters — is unchanged
// too. Without the palette in the key, a theme switch left the sidebar
// rendered in the colours it had before, until something unrelated
// happened to invalidate it.
func TestSidebarCacheInvalidatesOnThemeChange(t *testing.T) {
	t.Parallel()

	m, _ := newSidebarCacheTestUI(t)
	m.updateSidebarScrollState()
	before := m.sidebar.content
	require.NotEmpty(t, before)

	*m.com.Styles = styles.Theme("graphite-amber")
	m.themePreview.setLive("graphite-amber")
	m.updateSidebarScrollState()

	require.NotEqual(t, before, m.sidebar.content,
		"a theme switch must re-render the sidebar, not serve the old palette")
}
