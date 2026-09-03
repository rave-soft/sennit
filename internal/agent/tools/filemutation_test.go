package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

// denyingPermissions refuses every request — the mutation-test double for
// (a) below: a write tool must not write when permission is refused.
type denyingPermissions struct {
	permission.Service
}

func (denyingPermissions) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	return false, nil
}
func (denyingPermissions) ConfinedDir() string { return "" }

// TestWriteTool_PermissionDeniedDoesNotWriteFile is the mutation-test
// target for (a): temporarily short-circuiting applyFileMutation's
// permission check (e.g. treating every request as granted) makes this
// test fail, because the file would land on disk despite denial.
func TestWriteTool_PermissionDeniedDoesNotWriteFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "new.txt")
	tool := NewWriteTool(nil, denyingPermissions{}, &mockHistoryService{}, mockFileTrackerService{}, dir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  WriteToolName,
		Input: `{"file_path":"new.txt","content":"hello\n"}`,
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "a denied write must not report success")
	require.NoFileExists(t, target)
}

// TestWriteTool_PermissionDeniedDoesNotCreateParentDirs is the regression
// test for write.go's early ensureParentDir call: it used to create
// newdir/sub/ on disk before applyFileMutation ever reached the permission
// prompt, so a denied write for a path inside a not-yet-existing directory
// tree still left that tree behind. applyFileMutation already creates the
// parent AFTER approval (filemutation.go), which made the early call both
// redundant and premature; removing it means a denial leaves nothing on
// disk at all.
func TestWriteTool_PermissionDeniedDoesNotCreateParentDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "newdir", "sub", "new.txt")
	tool := NewWriteTool(nil, denyingPermissions{}, &mockHistoryService{}, mockFileTrackerService{}, dir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  WriteToolName,
		Input: `{"file_path":"newdir/sub/new.txt","content":"hello\n"}`,
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "a denied write must not report success")
	require.NoFileExists(t, target)
	require.NoDirExists(t, filepath.Join(dir, "newdir"), "a denied write must not leave the parent directory tree behind")
}

// staleFileTracker reports a fixed, caller-chosen LastReadTime instead of
// the "always just read it" behavior mockFileTrackerService gives, so a
// test can put a file's on-disk mtime after the session's last read of it.
type staleFileTracker struct {
	mockFileTrackerService
	lastRead time.Time
}

func (s staleFileTracker) LastReadTime(context.Context, string, string) time.Time {
	return s.lastRead
}

// TestWriteTool_StaleFileRejectsOverwrite is the mutation-test target for
// (c): temporarily making checkFileFreshness always report fileFresh makes
// this test fail, because the write would silently overwrite a file that
// changed under the model instead of refusing.
func TestWriteTool_StaleFileRejectsOverwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(target, []byte("original\n"), 0o644))

	// The file's mtime is "now" (just created above); lastRead is an hour
	// earlier, simulating a read that happened before something else wrote
	// to the file.
	tracker := staleFileTracker{lastRead: time.Now().Add(-time.Hour)}
	perms := &confinedTestPermissions{dir: ""}
	tool := NewWriteTool(nil, perms, &mockHistoryService{}, tracker, dir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  WriteToolName,
		Input: `{"file_path":"existing.txt","content":"overwritten\n"}`,
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "a write to a file modified since it was read must be refused")
	require.Contains(t, resp.Content, "changed on disk")

	onDisk, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk), "the stale write must not land")
}

// TestCheckFileFreshness_SubSecondWriteIsStale guards the resolution fix:
// read_at is now millisecond precision, so a write landing later in the
// same wall-clock second as the read must still be caught as stale.
func TestCheckFileFreshness_SubSecondWriteIsStale(t *testing.T) {
	t.Parallel()

	lastRead := time.Date(2026, 9, 3, 12, 0, 0, 100_000_000, time.UTC)
	modTime := lastRead.Add(400 * time.Millisecond) // same second, later.
	tracker := staleFileTracker{lastRead: lastRead}

	state, got := checkFileFreshness(context.Background(), tracker, "session", "file.txt", modTime)
	require.Equal(t, fileStale, state)
	require.Equal(t, lastRead, got)
}

// TestCheckFileFreshness_ReadThenImmediateEditNotStale is the regression
// guard for the common "read, then edit" flow: the edit's own write lands
// at or before the recorded read time (they can share a timestamp when
// both happen within the same tool call), so it must not be flagged stale.
func TestCheckFileFreshness_ReadThenImmediateEditNotStale(t *testing.T) {
	t.Parallel()

	lastRead := time.Date(2026, 9, 3, 12, 0, 0, 500_000_000, time.UTC)
	modTime := lastRead // the edit's mtime is not after the read time.
	tracker := staleFileTracker{lastRead: lastRead}

	state, _ := checkFileFreshness(context.Background(), tracker, "session", "file.txt", modTime)
	require.Equal(t, fileFresh, state)
}

// TestCheckFileFreshness_SameMillisecondWriteNotStale guards the other
// direction of the resolution fix: read_at is stored millisecond-aligned,
// so a write numerically after it but inside the same millisecond — the
// agent's own write landing right after the read/edit row update — must
// still read as fresh, not stale. Without truncating modTime to
// milliseconds before the comparison, this would misreport the agent's
// own follow-up edit as an external change.
func TestCheckFileFreshness_SameMillisecondWriteNotStale(t *testing.T) {
	t.Parallel()

	lastRead := time.Date(2026, 9, 3, 12, 0, 0, 100_000_000, time.UTC) // ms-aligned, as read_at is stored.
	modTime := lastRead.Add(400 * time.Microsecond)                    // numerically later, same millisecond.
	tracker := staleFileTracker{lastRead: lastRead}

	state, _ := checkFileFreshness(context.Background(), tracker, "session", "file.txt", modTime)
	require.Equal(t, fileFresh, state)
}

// TestWriteTool_DanglingSymlinkShowsResolvedTargetInPermissionDialog is the
// regression test for G20: req.filePath pointing at a dangling symlink made
// resolveExistingAncestorSymlinks fail (it fails closed on an unresolvable
// component rather than treat it as "doesn't exist yet"), and the error was
// swallowed by falling back to the unresolved link path. That path sits
// inside workingDir, so PathOrPrefix collapsed the permission key — and the
// dialog's Path — back to workingDir, even though the write itself follows
// the link to whatever it targets, which can be anywhere.
func TestWriteTool_DanglingSymlinkShowsResolvedTargetInPermissionDialog(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "workdir")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	outsideTarget := filepath.Join(root, "outside", "current.log")

	link := filepath.Join(workdir, "latest.log")
	require.NoError(t, os.Symlink(outsideTarget, link))

	perms := &recordingConfinedPermissions{confinedTestPermissions: &confinedTestPermissions{dir: ""}}
	tool := NewWriteTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workdir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  WriteToolName,
		Input: mustJSONInput(t, WriteParams{FilePath: "latest.log", Content: "hello\n"}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "the request is denied, but a denial is still the tool's normal path here")
	require.Len(t, perms.requests, 1)
	require.Equal(t, outsideTarget, perms.requests[0].Path,
		"the dialog must be keyed on the symlink's target, not the link itself")
	require.Contains(t, perms.requests[0].Description, outsideTarget,
		"the dialog must tell the user where the write actually resolves to")
}

// (b) — a confined workspace refusing a write outside itself even though
// permission would grant it — is already covered by
// TestWriteTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside in
// confinement_test.go; see the mutation-testing notes in this task's report
// for how that test fails when confinementRefusal is defeated.

// TestFinishMutation pins the shared tail edit, multiedit, write and
// lsp_replace_symbol all delegate to: an uncommitted failure and a
// model-visible error response both skip wrap and the LSP notification, a
// committed-but-later-failed mutation still notifies but returns the zero
// response with the error, and only an outright success wraps the body
// and appends diagnostics.
func TestFinishMutation(t *testing.T) {
	t.Parallel()

	noWrap := func(t *testing.T) func(string) string {
		return func(string) string {
			t.Helper()
			t.Fatal("wrap must not run on this path")
			return ""
		}
	}

	t.Run("uncommitted failure returns the response and error untouched", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("boom")
		wantResp := fantasy.ToolResponse{}
		resp, err := finishMutation(t.Context(), nil, "f.txt", wantResp, wantErr, noWrap(t))
		require.Equal(t, wantErr, err)
		require.Equal(t, wantResp, resp)
	})

	t.Run("a model-visible error response passes through untouched", func(t *testing.T) {
		t.Parallel()
		errResp := fantasy.NewTextErrorResponse("bad old_string")
		resp, err := finishMutation(t.Context(), nil, "f.txt", errResp, nil, noWrap(t))
		require.NoError(t, err)
		require.Equal(t, errResp, resp)
	})

	t.Run("a committed mutation followed by a later failure still notifies and returns the error", func(t *testing.T) {
		t.Parallel()
		wantErr := &committedMutationError{err: errors.New("history update failed")}
		resp, err := finishMutation(t.Context(), nil, "f.txt", fantasy.ToolResponse{Content: "done"}, wantErr, noWrap(t))
		require.Equal(t, wantErr, err)
		require.Equal(t, fantasy.ToolResponse{}, resp)
	})

	t.Run("success wraps the body and appends diagnostics", func(t *testing.T) {
		t.Parallel()
		resp, err := finishMutation(t.Context(), nil, "f.txt", fantasy.ToolResponse{Content: "ok"}, nil, func(content string) string {
			return "<result>\n" + content + "\n</result>\n"
		})
		require.NoError(t, err)
		// getDiagnostics returns "" for a nil manager, so nothing follows
		// the wrapped body here.
		require.Equal(t, "<result>\nok\n</result>\n", resp.Content)
	})
}
