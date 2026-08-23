package dialog

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newContentTestPermissions builds a Permissions dialog for the given tool
// name and params, ready to render content without going through Draw.
func newContentTestPermissions(t *testing.T, toolName string, params any) *Permissions {
	t.Helper()
	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	perm := permission.PermissionRequest{
		ID:         "perm-test",
		ToolCallID: "tool-call-test",
		ToolName:   toolName,
		Params:     params,
	}
	p := NewPermissions(com, perm)
	p.viewportDirty = true
	return p
}

// TestRenderContent_Dispatch verifies renderContent routes each tool name
// to the renderer that tool is supposed to use — the exact property a
// registry-based refactor of this switch could silently break.
func TestRenderContent_Dispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		toolName string
		params   any
		contains string
	}{
		{
			name:     "bash",
			toolName: proto.BashToolName,
			params:   proto.BashPermissionsParams{Command: "echo unique-bash-marker"},
			contains: "echo unique-bash-marker",
		},
		{
			name:     "edit",
			toolName: proto.EditToolName,
			params: proto.EditPermissionsParams{
				FilePath:   "edit.txt",
				OldContent: "old-edit-line\n",
				NewContent: "new-edit-line\n",
			},
			contains: "new-edit-line",
		},
		{
			name:     "write",
			toolName: proto.WriteToolName,
			params: proto.WritePermissionsParams{
				FilePath:   "write.txt",
				OldContent: "",
				NewContent: "new-write-line\n",
			},
			contains: "new-write-line",
		},
		{
			name:     "multiedit",
			toolName: proto.MultiEditToolName,
			params: proto.MultiEditPermissionsParams{
				FilePath:   "multiedit.txt",
				OldContent: "old-multi-line\n",
				NewContent: "new-multi-line\n",
			},
			contains: "new-multi-line",
		},
		{
			name:     "replace_symbol",
			toolName: proto.ReplaceSymbolToolName,
			params: proto.ReplaceSymbolPermissionsParams{
				FilePath:   "symbol.go",
				OldContent: "old-symbol-body\n",
				NewContent: "totally-different-symbol\n",
			},
			contains: "totally-different-symbol",
		},
		{
			name:     "download",
			toolName: proto.DownloadToolName,
			params: proto.DownloadPermissionsParams{
				URL:      "https://example.com/unique-download",
				FilePath: "out.bin",
			},
			contains: "https://example.com/unique-download",
		},
		{
			name:     "fetch",
			toolName: proto.FetchToolName,
			params:   proto.FetchPermissionsParams{URL: "https://example.com/unique-fetch"},
			contains: "https://example.com/unique-fetch",
		},
		{
			name:     "agentic_fetch",
			toolName: proto.AgenticFetchToolName,
			params: proto.AgenticFetchPermissionsParams{
				URL:    "https://example.com/unique-agentic-fetch",
				Prompt: "unique-agentic-prompt",
			},
			contains: "unique-agentic-prompt",
		},
		{
			name:     "read",
			toolName: proto.ReadToolName,
			params:   proto.ReadPermissionsParams{FilePath: "unique-read-file.go"},
			contains: "unique-read-file.go",
		},
		{
			name:     "ls",
			toolName: proto.LSToolName,
			params:   proto.LSPermissionsParams{Path: "unique-ls-dir"},
			contains: "unique-ls-dir",
		},
		{
			name:     "unknown tool falls back to default",
			toolName: "some_unknown_tool",
			params:   nil,
			contains: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := newContentTestPermissions(t, tc.toolName, tc.params)
			got := ansi.Strip(p.renderContent(80))
			if tc.contains != "" {
				require.Contains(t, got, tc.contains)
			}
		})
	}
}

// TestRenderContent_DefaultArm verifies the unrecognized-tool-name arm
// falls through to renderDefaultContent, which surfaces the description
// and pretty-printed params.
func TestRenderContent_DefaultArm(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	perm := permission.PermissionRequest{
		ID:          "perm-test",
		ToolCallID:  "tool-call-test",
		ToolName:    "totally_unknown_tool",
		Description: "unique-default-description",
	}
	p := NewPermissions(com, perm)
	p.viewportDirty = true

	got := p.renderContent(80)
	require.Contains(t, got, "unique-default-description")
}

// TestRenderContent_WrongParamsTypeYieldsEmpty pins the type-assertion
// guard at the top of each diff renderer: pairing a tool name with the
// wrong params type must produce an empty string, not a panic. This is
// exactly the failure a mis-wired renderer registry would produce.
func TestRenderContent_WrongParamsTypeYieldsEmpty(t *testing.T) {
	t.Parallel()

	wrongParams := proto.LSPermissionsParams{Path: "not-a-diff-params-type"}

	tests := []struct {
		name     string
		toolName string
	}{
		{"edit", proto.EditToolName},
		{"write", proto.WriteToolName},
		{"multiedit", proto.MultiEditToolName},
		{"replace_symbol", proto.ReplaceSymbolToolName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := newContentTestPermissions(t, tc.toolName, wrongParams)
			require.NotPanics(t, func() {
				got := p.renderContent(80)
				require.Empty(t, got)
			})
		})
	}
}

// TestDiffContentRenderer_GuardStopsBeforeToDiff pins the type guard in
// diffContentRenderer itself, which TestRenderContent_WrongParamsTypeYieldsEmpty
// cannot do. That test asserts only that the rendered output is empty, and a
// bypassed guard also renders empty: it falls through with a zero-value P, so
// renderDiff gets three empty strings and produces nothing either way. The
// observable difference is not the output but whether toDiff was reached at
// all, so this asserts on that directly.
func TestDiffContentRenderer_GuardStopsBeforeToDiff(t *testing.T) {
	t.Parallel()

	t.Run("mismatched type never reaches toDiff", func(t *testing.T) {
		t.Parallel()

		called := false
		render := diffContentRenderer(func(proto.EditPermissionsParams) diffParams {
			called = true
			return diffParams{}
		})

		p := newContentTestPermissions(t, proto.EditToolName,
			proto.LSPermissionsParams{Path: "not-a-diff-params-type"})

		require.Empty(t, render(p, 80))
		require.False(t, called,
			"the type guard must return before toDiff runs; reaching toDiff with a "+
				"zero-value P renders empty too, which is why output alone cannot "+
				"tell a working guard from a bypassed one")
	})

	t.Run("matching type does reach toDiff", func(t *testing.T) {
		t.Parallel()

		called := false
		render := diffContentRenderer(func(params proto.EditPermissionsParams) diffParams {
			called = true
			return diffParams{
				filePath:   params.FilePath,
				oldContent: "old\n",
				newContent: "new\n",
			}
		})

		p := newContentTestPermissions(t, proto.EditToolName,
			proto.EditPermissionsParams{FilePath: "a.go"})

		require.NotEmpty(t, render(p, 80))
		require.True(t, called, "a matching params type must reach toDiff")
	})
}

// TestRenderDiff_SplitVsUnified verifies isSplitMode/WithDiffMode decide
// which cached diff body renderDiff produces, and that both bodies contain
// the changed content.
func TestRenderDiff_SplitVsUnified(t *testing.T) {
	t.Parallel()

	params := proto.EditPermissionsParams{
		FilePath:   "diffmode.txt",
		OldContent: "before-line\n",
		NewContent: "after-line\n",
	}

	t.Run("explicit split mode", func(t *testing.T) {
		t.Parallel()
		s := styles.SennitDark()
		com := &common.Common{Styles: &s}
		perm := permission.PermissionRequest{ToolName: proto.EditToolName, Params: params}
		p := NewPermissions(com, perm, WithDiffMode(true))
		p.viewportDirty = true

		require.True(t, p.isSplitMode())
		out := p.renderContent(120)
		require.Contains(t, out, "after-line")
		require.Equal(t, p.splitDiffContent, out)
		require.Empty(t, p.unifiedDiffContent)
	})

	t.Run("explicit unified mode", func(t *testing.T) {
		t.Parallel()
		s := styles.SennitDark()
		com := &common.Common{Styles: &s}
		perm := permission.PermissionRequest{ToolName: proto.EditToolName, Params: params}
		p := NewPermissions(com, perm, WithDiffMode(false))
		p.viewportDirty = true

		require.False(t, p.isSplitMode())
		out := p.renderContent(120)
		require.Contains(t, out, "after-line")
		require.Equal(t, p.unifiedDiffContent, out)
		require.Empty(t, p.splitDiffContent)
	})

	t.Run("default mode follows width via defaultDiffSplitMode", func(t *testing.T) {
		t.Parallel()
		s := styles.SennitDark()
		com := &common.Common{Styles: &s}
		perm := permission.PermissionRequest{ToolName: proto.EditToolName, Params: params}
		p := NewPermissions(com, perm)
		p.viewportDirty = true

		// No explicit mode: isSplitMode reads defaultDiffSplitMode, which
		// Draw normally sets from the dialog width. Set it directly to
		// exercise both branches without going through Draw.
		p.defaultDiffSplitMode = true
		require.True(t, p.isSplitMode())

		p.defaultDiffSplitMode = false
		require.False(t, p.isSplitMode())
	})

	t.Run("cached content reused when viewport not dirty", func(t *testing.T) {
		t.Parallel()
		s := styles.SennitDark()
		com := &common.Common{Styles: &s}
		perm := permission.PermissionRequest{ToolName: proto.EditToolName, Params: params}
		p := NewPermissions(com, perm, WithDiffMode(false))
		p.viewportDirty = true
		first := p.renderContent(120)

		// Not dirty: renderDiff should return the cached body without
		// recomputing, even though we hand it different arguments.
		p.viewportDirty = false
		cached := p.renderDiff("different.txt", "unrelated old", "unrelated new", 120)
		require.Equal(t, first, cached)
	})
}

// TestScrollLeftRight verifies the horizontal scroll helpers step by
// horizontalScrollStep and clamp at the left edge (scrollRight has no
// upper clamp).
func TestScrollLeftRight(t *testing.T) {
	t.Parallel()

	p := newContentTestPermissions(t, proto.EditToolName, proto.EditPermissionsParams{
		FilePath:   "scroll.txt",
		OldContent: "a\n",
		NewContent: "b\n",
	})

	require.Equal(t, 0, p.diffXOffset)

	// Clamped at zero: cannot scroll left past the start.
	p.scrollLeft()
	require.Equal(t, 0, p.diffXOffset)
	require.True(t, p.viewportDirty)

	p.viewportDirty = false
	p.scrollRight()
	require.Equal(t, horizontalScrollStep, p.diffXOffset)
	require.True(t, p.viewportDirty)

	p.scrollRight()
	require.Equal(t, 2*horizontalScrollStep, p.diffXOffset)

	p.scrollLeft()
	require.Equal(t, horizontalScrollStep, p.diffXOffset)

	p.scrollLeft()
	require.Equal(t, 0, p.diffXOffset)

	// Further scrollLeft calls stay clamped at zero.
	p.scrollLeft()
	require.Equal(t, 0, p.diffXOffset)
}

// TestRenderDiff_HorizontalScrollShiftsContent pins the fix for the bug
// where left/right scrolling a permission diff did nothing: renderDiff
// always passed WrapLines(true), and the wrapped renderers in diffview.go
// ignore XOffset entirely, so a non-zero p.diffXOffset never changed the
// rendered output. renderDiff now wraps only while unscrolled
// (WrapLines(p.diffXOffset == 0)), so scrolling right switches to the
// non-wrapped renderer, which does honour XOffset. Covers both split and
// unified modes, since each keeps its own cache
// (splitDiffContent/unifiedDiffContent).
func TestRenderDiff_HorizontalScrollShiftsContent(t *testing.T) {
	t.Parallel()

	// A line much wider than the render width, so there is content to
	// scroll to that a truncated, unscrolled view would not show.
	params := proto.EditPermissionsParams{
		FilePath:   "wide.txt",
		OldContent: "const value = \"aaaaaaaaaa-old-marker-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"\n",
		NewContent: "const value = \"aaaaaaaaaa-new-marker-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"\n",
	}

	for _, tc := range []struct {
		name  string
		split bool
	}{
		{"unified", false},
		{"split", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := styles.SennitDark()
			com := &common.Common{Styles: &s}
			perm := permission.PermissionRequest{ToolName: proto.EditToolName, Params: params}
			p := NewPermissions(com, perm, WithDiffMode(tc.split))
			p.viewportDirty = true

			const width = 40
			atZero := p.renderDiff(params.FilePath, params.OldContent, params.NewContent, width)

			p.scrollRight() // sets diffXOffset and marks viewportDirty
			require.NotZero(t, p.diffXOffset)
			scrolled := p.renderDiff(params.FilePath, params.OldContent, params.NewContent, width)

			require.NotEqual(t, atZero, scrolled,
				"scrolling right must change the rendered diff content")

			// Scrolling back to zero must restore the original (wrapped)
			// rendering, proving the cache was invalidated both ways.
			p.scrollLeft() // one step of horizontalScrollStep returns to 0
			require.Zero(t, p.diffXOffset)
			backToZero := p.renderDiff(params.FilePath, params.OldContent, params.NewContent, width)
			require.Equal(t, atZero, backToZero,
				"scrolling back to zero must restore the wrapped rendering")
		})
	}
}

// TestRenderContentPanel verifies renderContentPanel renders the given
// content at the requested width using the dialog's content-panel style.
func TestRenderContentPanel(t *testing.T) {
	t.Parallel()

	p := newContentTestPermissions(t, proto.BashToolName, proto.BashPermissionsParams{Command: "x"})
	got := p.renderContentPanel("unique-panel-content", 40)
	require.Contains(t, got, "unique-panel-content")
}

// TestSmallAccessors covers the small public accessors renderContent's
// planned registry refactor must preserve unchanged.
func TestSmallAccessors(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	require.Equal(t, PermissionsID, p.ID())
	require.Equal(t, "tool-call-test", p.ToolCallID())

	full := p.FullHelp()
	require.Len(t, full, 1)
	require.Equal(t, p.ShortHelp(), full[0])
}

// TestPrettyName covers the snake_case/kebab-case-to-Title-Case conversion
// used for MCP tool name display.
func TestPrettyName(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Foo Bar", prettyName("foo_bar"))
	require.Equal(t, "Foo Bar", prettyName("foo-bar"))
	require.Equal(t, "Foobar", prettyName("foobar"))
}

// TestDelegationHeaderKey verifies the "who is asking" label matches the
// delegation kind, falling back to a generic label for unknown kinds.
func TestDelegationHeaderKey(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Thread", delegationHeaderKey(string(proto.ThreadKindThread)))
	require.Equal(t, "Task", delegationHeaderKey(string(proto.ThreadKindTask)))
	require.Equal(t, "From", delegationHeaderKey("something-else"))
	require.Equal(t, "From", delegationHeaderKey(""))
}

// TestDelegationHeaderValue verifies the value prefers the delegation's
// name, falling back to its ID when unnamed.
func TestDelegationHeaderValue(t *testing.T) {
	t.Parallel()

	require.Equal(t, "my-name", delegationHeaderValue(permission.DelegationRef{ID: "id-1", Name: "my-name"}))
	require.Equal(t, "id-1", delegationHeaderValue(permission.DelegationRef{ID: "id-1"}))
}

// TestRenderContent_DownloadTimeout verifies the download renderer includes
// the optional timeout line only when set.
func TestRenderContent_DownloadTimeout(t *testing.T) {
	t.Parallel()

	p := newContentTestPermissions(t, proto.DownloadToolName, proto.DownloadPermissionsParams{
		URL:      "https://example.com/f",
		FilePath: "f.bin",
		Timeout:  42,
	})
	got := p.renderContent(80)
	require.Contains(t, got, "42s")
}

// TestRenderContent_AgenticFetchNoURL verifies the agentic-fetch renderer
// omits the URL line when the params carry none.
func TestRenderContent_AgenticFetchNoURL(t *testing.T) {
	t.Parallel()

	p := newContentTestPermissions(t, proto.AgenticFetchToolName, proto.AgenticFetchPermissionsParams{
		Prompt: "unique-prompt-only",
	})
	got := p.renderContent(80)
	require.Contains(t, got, "unique-prompt-only")
	require.False(t, strings.Contains(got, "URL:"))
}

// TestRenderContent_ViewOffsetAndLimit verifies the read/view renderer adds
// the offset and limit lines only when they diverge from defaults.
func TestRenderContent_ViewOffsetAndLimit(t *testing.T) {
	t.Parallel()

	p := newContentTestPermissions(t, proto.ReadToolName, proto.ReadPermissionsParams{
		FilePath: "f.go",
		Offset:   10,
		Limit:    50,
	})
	got := p.renderContent(80)
	require.Contains(t, got, "Starting from line: 11")
	require.Contains(t, got, "Lines to read: 50")
}

// TestRenderContent_LSIgnorePatterns verifies the ls renderer joins ignore
// patterns into the panel when present.
func TestRenderContent_LSIgnorePatterns(t *testing.T) {
	t.Parallel()

	p := newContentTestPermissions(t, proto.LSToolName, proto.LSPermissionsParams{
		Path:   "dir",
		Ignore: []string{"*.log", "node_modules"},
	})
	got := p.renderContent(80)
	require.Contains(t, got, "*.log, node_modules")
}
