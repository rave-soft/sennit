package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

type mockPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (m *mockPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (m *mockPermissionService) GrantPersistent(req permission.PermissionRequest) bool {
	return true
}

func (m *mockPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockPermissionService) SetSkipRequests(skip bool) {}

func (m *mockPermissionService) SkipRequests() bool {
	return false
}

func (m *mockPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

type mockHistoryService struct{}

func (*mockHistoryService) CreateVersion(context.Context, string, string, string) error {
	return nil
}

func (*mockHistoryService) LatestContent(context.Context, string, string) (string, bool, error) {
	return "", true, nil
}

var _ FileHistory = (*mockHistoryService)(nil)

func TestApplyEditToContentPartialSuccess(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\nline 3\n"

	// Test successful edit.
	newContent, _, err := applyEditToContent(content, MultiEditOperation{
		OldString: "line 1",
		NewString: "LINE 1",
	})
	require.NoError(t, err)
	require.Contains(t, newContent, "LINE 1")
	require.Contains(t, newContent, "line 2")

	// Test failed edit (string not found).
	_, _, err = applyEditToContent(content, MultiEditOperation{
		OldString: "line 99",
		NewString: "LINE 99",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestApplyEditToContentReplacementModes(t *testing.T) {
	t.Parallel()

	content := "alpha\nbeta\nalpha\n"

	newContent, _, err := applyEditToContent(content, MultiEditOperation{
		OldString:  "alpha",
		NewString:  "ALPHA",
		ReplaceAll: true,
	})
	require.NoError(t, err)
	require.Equal(t, "ALPHA\nbeta\nALPHA\n", newContent)

	_, _, err = applyEditToContent(content, MultiEditOperation{
		OldString: "alpha",
		NewString: "ALPHA",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "multiple times")

	newContent, _, err = applyEditToContent(content, MultiEditOperation{})
	require.NoError(t, err)
	require.Equal(t, content, newContent)
}

func TestMultiEditSequentialApplication(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	// Create test file.
	content := "line 1\nline 2\nline 3\nline 4\n"
	err := os.WriteFile(testFile, []byte(content), 0o644)
	require.NoError(t, err)

	// Manually test the sequential application logic.
	currentContent := content

	// Apply edits sequentially, tracking failures.
	edits := []MultiEditOperation{
		{OldString: "line 1", NewString: "LINE 1"},   // Should succeed
		{OldString: "line 99", NewString: "LINE 99"}, // Should fail - doesn't exist
		{OldString: "line 3", NewString: "LINE 3"},   // Should succeed
		{OldString: "line 2", NewString: "LINE 2"},   // Should succeed - still exists
	}

	var failedEdits []FailedEdit
	successCount := 0

	for i, edit := range edits {
		newContent, _, err := applyEditToContent(currentContent, edit)
		if err != nil {
			failedEdits = append(failedEdits, FailedEdit{
				Index: i + 1,
				Error: err.Error(),
				Edit:  edit,
			})
			continue
		}
		currentContent = newContent
		successCount++
	}

	// Verify results.
	require.Equal(t, 3, successCount, "Expected 3 successful edits")
	require.Len(t, failedEdits, 1, "Expected 1 failed edit")

	// Check failed edit details.
	require.Equal(t, 2, failedEdits[0].Index)
	require.Contains(t, failedEdits[0].Error, "not found")

	// Verify content changes.
	require.Contains(t, currentContent, "LINE 1")
	require.Contains(t, currentContent, "LINE 2")
	require.Contains(t, currentContent, "LINE 3")
	require.Contains(t, currentContent, "line 4") // Original unchanged
	require.NotContains(t, currentContent, "LINE 99")
}

func TestMultiEditAllEditsSucceed(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\nline 3\n"

	edits := []MultiEditOperation{
		{OldString: "line 1", NewString: "LINE 1"},
		{OldString: "line 2", NewString: "LINE 2"},
		{OldString: "line 3", NewString: "LINE 3"},
	}

	currentContent := content
	successCount := 0

	for _, edit := range edits {
		newContent, _, err := applyEditToContent(currentContent, edit)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		currentContent = newContent
		successCount++
	}

	require.Equal(t, 3, successCount)
	require.Contains(t, currentContent, "LINE 1")
	require.Contains(t, currentContent, "LINE 2")
	require.Contains(t, currentContent, "LINE 3")
}

func TestMultiEditAllEditsFail(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\n"

	edits := []MultiEditOperation{
		{OldString: "line 99", NewString: "LINE 99"},
		{OldString: "line 100", NewString: "LINE 100"},
	}

	currentContent := content
	var failedEdits []FailedEdit

	for i, edit := range edits {
		newContent, _, err := applyEditToContent(currentContent, edit)
		if err != nil {
			failedEdits = append(failedEdits, FailedEdit{
				Index: i + 1,
				Error: err.Error(),
				Edit:  edit,
			})
			continue
		}
		currentContent = newContent
	}

	require.Len(t, failedEdits, 2)
	require.Equal(t, content, currentContent, "Content should be unchanged")
}

// TestFormatFailedEditReasonsCapsOutput is the regression test for finding
// 6: formatFailedEditReasons rendered one full reason line per failed
// edit with no cap, and each reason can carry a several-line
// diagnoseMismatch window of real file content — measured on a batch of
// 40 failed edits against a 200-line file at 26KB, against 45 bytes
// before the fix that made every candidate get queried added the
// per-edit reason in the first place. A batch this size must still name
// a handful of edits and then say how many more failed, not repeat the
// same shape 40 times over.
func TestFormatFailedEditReasonsCapsOutput(t *testing.T) {
	t.Parallel()

	// Stand-in for a realistic diagnoseMismatch hint, which carries a
	// several-line window of real file content per failure — simulated
	// directly here rather than depending on its fuzzy-match heuristics
	// picking a particular file layout.
	window := strings.Repeat("  123 | some line of file content near the mismatch\n", 10)
	const total = 40
	failed := make([]FailedEdit, total)
	for i := range failed {
		failed[i] = FailedEdit{
			Index: i + 1,
			Error: "old_string not found in file. Make sure it matches exactly, including whitespace and line breaks\n\n" + window,
		}
	}

	out := formatFailedEditReasons(failed)

	require.Contains(t, out, "edit 1:")
	require.Contains(t, out, fmt.Sprintf("edit %d:", maxFailedEditReasons))
	require.NotContains(t, out, fmt.Sprintf("edit %d:", maxFailedEditReasons+1),
		"only the first %d failures get their own reason line", maxFailedEditReasons)
	require.Contains(t, out, fmt.Sprintf("... and %d more failed edit(s)", total-maxFailedEditReasons))
	require.Less(t, len(out), 8000,
		"40 failures at ~400 bytes each must not make the response grow linearly with the batch size")
}

func TestProcessMultiEditExistingFilePartialFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("one\ntwo\nthree\n"), 0o644))

	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: &mockEditFileTracker{lastRead: time.Now().Add(time.Second)},
		workingDir:  dir,
	}
	params := MultiEditParams{
		FilePath: filePath,
		Edits: []MultiEditOperation{
			{OldString: "two", NewString: "TWO"},
			{OldString: "missing", NewString: "MISSING"},
		},
	}

	resp, err := processMultiEditExistingFile(edit, params, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Applied 1 of 2 edits")
	// Finding C: the model only ever sees resp.Content, not the metadata's
	// EditsFailed — the reason a failed edit didn't apply must be in the
	// text itself, not just in a struct rendered for humans.
	require.Contains(t, resp.Content, "edit 2:", "the failure reason must name which edit failed")
	require.Contains(t, resp.Content, "old_string not found in file", "the failure reason must say why the edit didn't apply")

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "one\nTWO\nthree\n", string(content))

	var meta MultiEditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 1, meta.EditsApplied)
	require.Len(t, meta.EditsFailed, 1)
	require.Equal(t, 2, meta.EditsFailed[0].Index)
	require.Equal(t, "one\ntwo\nthree\n", meta.OldContent)
	require.Equal(t, "one\nTWO\nthree\n", meta.NewContent)
}

func TestProcessMultiEditWithCreationPartialFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "nested", "test.txt")

	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: &mockEditFileTracker{},
		workingDir:  dir,
	}
	params := MultiEditParams{
		FilePath: filePath,
		Edits: []MultiEditOperation{
			{OldString: "", NewString: "one\ntwo\nthree\n"},
			{OldString: "two", NewString: "TWO"},
			{OldString: "missing", NewString: "MISSING"},
		},
	}

	resp, err := processMultiEditWithCreation(edit, params, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "File created with 2 of 3 edits")
	require.Contains(t, resp.Content, "edit 3:", "the failure reason must name which edit failed")
	require.Contains(t, resp.Content, "old_string not found in file", "the failure reason must say why the edit didn't apply")

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "one\nTWO\nthree\n", string(content))

	var meta MultiEditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 2, meta.EditsApplied)
	require.Len(t, meta.EditsFailed, 1)
	require.Equal(t, 3, meta.EditsFailed[0].Index)
	require.Equal(t, "", meta.OldContent)
	require.Equal(t, "one\nTWO\nthree\n", meta.NewContent)
}

func (m *mockPermissionService) ActiveRequest() (permission.PermissionRequest, bool) {
	return permission.PermissionRequest{}, false
}

func (m *mockPermissionService) AwaitingAnswer(string) bool { return false }

func (*mockPermissionService) ConfineToWorkingDir() {}
func (*mockPermissionService) ConfinedDir() string  { return "" }
