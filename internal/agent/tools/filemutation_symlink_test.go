package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// symlinkOrSkip creates dest -> link, skipping the test when the platform
// cannot create symlinks (matches the guard in
// internal/fsext/glob_symlink_test.go).
func symlinkOrSkip(t *testing.T, dest, link string) {
	t.Helper()
	if err := os.Symlink(dest, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

// recordingHistory records the file path every CreateVersion call is made
// against, so a test can assert history was recorded for the file that
// actually changed rather than the name the mutation was asked to edit.
type recordingHistory struct {
	mockHistoryService
	paths []string
}

func (h *recordingHistory) CreateVersion(_ context.Context, _, filePath, _ string) error {
	h.paths = append(h.paths, filePath)
	return nil
}

func editMutation(filePath, newContent string) fileMutationRequest {
	return fileMutationRequest{
		editContext: editContext{ctx: context.Background()},
		call:        fantasy.ToolCall{ID: "call"},
		filePath:    filePath,
		sessionID:   "session",
		toolName:    EditToolName,
		prepare: func(fileSnapshot) (preparedFileMutation, error) {
			return preparedFileMutation{
				diffContent: newContent, writeContent: newContent,
				description: "edit", permParams: EditPermissionsParams{}, successMessage: "done",
				metadata: func(string, string, int, int) any { return EditResponseMetadata{} },
			}, nil
		},
	}
}

// TestApplyFileMutation_ThroughSymlink_WritesTargetAndKeepsLink pins the
// fix for defect A: editing a file reached through a symlink must land on
// the link's target, not replace the link itself, and history must be
// recorded under the file that actually changed.
func TestApplyFileMutation_ThroughSymlink_WritesTargetAndKeepsLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("old"), 0o644))
	link := filepath.Join(dir, "link.txt")
	symlinkOrSkip(t, real, link)

	history := &recordingHistory{}
	req := editMutation(link, "new")
	req.files = history
	req.workingDir = dir

	resp, err := applyFileMutation(req)
	require.NoError(t, err)
	require.False(t, resp.IsError)

	content, readErr := os.ReadFile(real)
	require.NoError(t, readErr)
	require.Equal(t, "new", string(content), "the write must land on the link's target")

	info, statErr := os.Lstat(link)
	require.NoError(t, statErr)
	require.True(t, info.Mode()&os.ModeSymlink != 0, "the link must still be a symlink, not a regular file")
	dest, readlinkErr := os.Readlink(link)
	require.NoError(t, readlinkErr)
	require.Equal(t, real, dest, "the link must still point at its original target")

	require.NotEmpty(t, history.paths)
	for _, p := range history.paths {
		require.Equal(t, real, p, "history must be recorded under the file that actually changed")
	}
}

// TestApplyFileMutation_ThroughDanglingSymlink_CreatesTargetWithoutLooping
// pins the fix for defect B: writing through a dangling symlink used to
// return ErrFileChanged forever (AtomicCreateFile's os.Link failing EEXIST
// against the symlink's own name). It must now create the link's target
// and succeed.
func TestApplyFileMutation_ThroughDanglingSymlink_CreatesTargetWithoutLooping(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "logs", "current.log")
	link := filepath.Join(dir, "latest.log")
	symlinkOrSkip(t, real, link)

	history := &recordingHistory{}
	req := editMutation(link, "first run\n")
	req.files = history
	req.workingDir = dir

	resp, err := applyFileMutation(req)
	require.NoError(t, err)
	require.False(t, resp.IsError, "resp: %+v", resp)

	content, readErr := os.ReadFile(link)
	require.NoError(t, readErr)
	require.Equal(t, "first run\n", string(content))

	info, statErr := os.Lstat(link)
	require.NoError(t, statErr)
	require.True(t, info.Mode()&os.ModeSymlink != 0, "the link itself must be untouched")

	require.NotEmpty(t, history.paths)
	for _, p := range history.paths {
		require.Equal(t, real, p, "history must be recorded under the file that was created")
	}
}
