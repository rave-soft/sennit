package tools

import (
	"context"
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

// (b) — a confined workspace refusing a write outside itself even though
// permission would grant it — is already covered by
// TestWriteTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside in
// confinement_test.go; see the mutation-testing notes in this task's report
// for how that test fails when confinementRefusal is defeated.
