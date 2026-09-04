package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newLSPToolRenameEditWorktree builds a fixture for a rename that touches
// two files, each with "Exact" at the same column on line 3 (1-based) so
// the "rename-edit" scenario's canned textDocument/rename response lines
// up with real file content: a.go defines Exact, b.go calls it.
func newLSPToolRenameEditWorktree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package e2e\n\nfunc Exact() string { return \"ok\" }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"), []byte("package e2e\n\nfunc B() string { return Exact() }\n"), 0o644))
	return root
}

// recordingRenameFileTracker is a per-path FileTracking stub: unlike
// mockEditFileTracker (edit_test.go), which folds every path into one
// FileCoverage, this one keeps calls separated by path so a test can check
// that a multi-file rename records each file's own edited span rather than
// merging them or, worse, recording a full read.
type recordingRenameFileTracker struct {
	mu    sync.Mutex
	reads []string
	edits []recordedEdit
}

type recordedEdit struct {
	path       string
	start, end int
}

func (t *recordingRenameFileTracker) RecordRead(ctx context.Context, sessionID, path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reads = append(t.reads, path)
}

func (t *recordingRenameFileTracker) RecordPartialRead(ctx context.Context, sessionID, path string, start, end int) {
}

func (t *recordingRenameFileTracker) RecordEdit(ctx context.Context, sessionID, path string, start, end, newEnd int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.edits = append(t.edits, recordedEdit{path: path, start: start, end: newEnd})
}

func (t *recordingRenameFileTracker) ReadCoverage(ctx context.Context, sessionID, path string) FileCoverage {
	return FileCoverage{}
}

func (t *recordingRenameFileTracker) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return time.Time{}
}

var _ FileTracking = (*recordingRenameFileTracker)(nil)

// TestLSPRenameThroughManagerRecordsEditedSpansNotWholeFileReads is the
// regression test for defect 2: lsp_rename used to call
// filetracker.RecordRead for every affected file after applying the
// rename, marking each one fully read even though the model only ever
// supplied a symbol and a new name for it, not their contents. That
// retroactively opened every renamed file to a blind edit anywhere in it.
func TestLSPRenameThroughManagerRecordsEditedSpansNotWholeFileReads(t *testing.T) {
	root := newLSPToolRenameEditWorktree(t)
	manager := newLSPToolE2EManager(t, root, "rename-edit")
	tracker := &recordingRenameFileTracker{}
	tool := NewRenameTool(manager, &mockPermissionService{}, &mockHistoryService{}, tracker, root)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "rename-session")

	resp := runToolWith(t, tool, ctx, RenameToolName, RenameParams{Symbol: "Exact", NewName: "Renamed", Path: "."})
	require.False(t, resp.IsError, resp.Content)

	aContent, err := os.ReadFile(filepath.Join(root, "a.go"))
	require.NoError(t, err)
	require.Contains(t, string(aContent), "func Renamed() string")
	bContent, err := os.ReadFile(filepath.Join(root, "b.go"))
	require.NoError(t, err)
	require.Contains(t, string(bContent), "return Renamed()")

	// No whole-file read was ever recorded for either affected file.
	require.Empty(t, tracker.reads)

	// Each affected file got its own recorded span, not one merged across
	// both files, and neither span reaches past the single line that
	// actually changed (line 3 of each 3-line fixture).
	require.Len(t, tracker.edits, 2)
	seen := map[string]recordedEdit{}
	for _, e := range tracker.edits {
		seen[e.path] = e
	}
	aEdit, ok := seen[filepath.Join(root, "a.go")]
	require.True(t, ok, "a.go must have a recorded edit span")
	require.Equal(t, 3, aEdit.start)
	require.Equal(t, 3, aEdit.end)
	bEdit, ok := seen[filepath.Join(root, "b.go")]
	require.True(t, ok, "b.go must have a recorded edit span")
	require.Equal(t, 3, bEdit.start)
	require.Equal(t, 3, bEdit.end)
}

// TestLSPRenameRefusesWhenResyncFails is the regression test for finding
// 1.6: resyncOpenFiles used to swallow NotifyChange errors and report
// success ("false") whenever every resync it attempted failed — the exact
// class the caller could not tell apart from "nothing was open." That let
// lsp_rename fall through to applying edits computed against a stale
// overlay instead of recomputing against disk. a.go is opened on the
// client, then deleted before the rename runs, so NotifyChange fails to
// read it and resyncOpenFiles must return an error the tool surfaces
// instead of silently proceeding.
func TestLSPRenameRefusesWhenResyncFails(t *testing.T) {
	root := newLSPToolRenameEditWorktree(t)
	manager := newLSPToolE2EManager(t, root, "rename-edit")

	aPath := filepath.Join(root, "a.go")
	manager.Start(t.Context(), aPath)
	client := findLSPClient(manager, aPath)
	require.NotNil(t, client)
	require.NoError(t, client.OpenFileOnDemand(t.Context(), aPath))
	require.True(t, client.IsFileOpen(aPath))

	require.NoError(t, os.Remove(aPath))

	tool := NewRenameTool(manager, &mockPermissionService{}, &mockHistoryService{}, nil, root)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "rename-session")

	resp := runToolWith(t, tool, ctx, RenameToolName, RenameParams{Symbol: "Exact", NewName: "Renamed", Path: "."})
	require.True(t, resp.IsError, "a failed resync must be surfaced as an error, not silently ignored")

	bContent, err := os.ReadFile(filepath.Join(root, "b.go"))
	require.NoError(t, err)
	require.NotContains(t, bContent, "Renamed", "no edit may be applied once the resync it depends on failed")
}

// TestLSPRenameThroughManagerRefusesConfinementBeforeRequestingPermission
// is the regression test for defect 3: the confinement check ran after the
// permission request, so a rename that would ultimately be refused for
// writing outside the workspace first interrupted the user with a prompt
// anyway. permissions.requests must stay empty here.
func TestLSPRenameThroughManagerRefusesConfinementBeforeRequestingPermission(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "rename-outside")
	permissions := &recordingConfinedPermissions{confinedTestPermissions: &confinedTestPermissions{dir: root}}
	tool := NewRenameTool(manager, permissions, nil, nil, root)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "rename-session")

	resp := runToolWith(t, tool, ctx, RenameToolName, RenameParams{Symbol: "Exact", NewName: "Renamed", Path: "."})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "outside this workspace")
	require.Empty(t, permissions.requests, "confinement must refuse before any permission request is made")
}
