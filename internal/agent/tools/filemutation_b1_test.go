package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

type failingHistory struct {
	mockHistoryService
}

func (*failingHistory) CreateVersion(context.Context, string, string, string) error {
	return errors.New("history unavailable")
}

type recordingTracker struct {
	mockFileTrackerService
	reads int
}

func (tracker *recordingTracker) RecordRead(context.Context, string, string) {
	tracker.reads++
}

var _ FileTracking = (*recordingTracker)(nil)

type snapshotChangingTracker struct {
	mockEditFileTracker
	mutate sync.Once
	path   string
	t      *testing.T
}

func (tracker *snapshotChangingTracker) ReadCoverage(context.Context, string, string) FileCoverage {
	tracker.mutate.Do(func() {
		require.NoError(tracker.t, os.WriteFile(tracker.path, []byte("one\nexternal\nthree\n"), 0o644))
	})
	return FileCoverage{Full: true}
}

type recordingMutationPermissions struct {
	mockPermissionService
	request permission.CreatePermissionRequest
}

func (service *recordingMutationPermissions) Request(_ context.Context, request permission.CreatePermissionRequest) (bool, error) {
	service.request = request
	return true, nil
}

func TestMultiEditUsesAuthoritativeSnapshotForPermissionAndResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644))
	tracker := &snapshotChangingTracker{mockEditFileTracker: mockEditFileTracker{lastRead: time.Now().Add(time.Second)}, path: path, t: t}
	permissions := &recordingMutationPermissions{}
	response, err := processMultiEditExistingFile(editContext{
		ctx: context.WithValue(confinedTestCtx(t), SessionIDContextKey, "session"), permissions: permissions,
		files: &mockHistoryService{}, filetracker: tracker, workingDir: dir,
	}, MultiEditParams{FilePath: path, Edits: []MultiEditOperation{
		{OldString: "one", NewString: "ONE"},
		{OldString: "two", NewString: "TWO"},
	}}, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Contains(t, permissions.request.Description, "Apply 1 of 2 edits")
	permissionParams, ok := permissions.request.Params.(MultiEditPermissionsParams)
	require.True(t, ok)
	require.Equal(t, "one\nexternal\nthree\n", permissionParams.OldContent)
	require.Equal(t, "ONE\nexternal\nthree\n", permissionParams.NewContent)
	require.Contains(t, response.Content, "Applied 1 of 2 edits")
	var metadata MultiEditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.Equal(t, 1, metadata.EditsApplied)
	require.Len(t, metadata.EditsFailed, 1)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "ONE\nexternal\nthree\n", string(content))
}

func TestApplyFileMutationRejectsChangeDuringApproval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	permissions := &mutatingPermissionService{mutate: func() { require.NoError(t, os.WriteFile(path, []byte("external"), 0o644)) }}
	response, err := applyFileMutation(fileMutationRequest{
		editContext: editContext{ctx: confinedTestCtx(t), permissions: permissions, workingDir: dir},
		call:        fantasy.ToolCall{ID: "call"}, filePath: path, sessionID: "session", toolName: EditToolName,
		prepare: func(snapshot fileSnapshot) (preparedFileMutation, error) {
			return preparedFileMutation{diffContent: "new", writeContent: "new", description: "edit", permParams: EditPermissionsParams{}, successMessage: "done", metadata: func(string, string, int, int) any { return EditResponseMetadata{} }}, nil
		},
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "external", string(content))
}

func TestApplyFileMutationTracksCommittedCreateHistoryFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	tracker := &recordingTracker{}
	_, err := applyFileMutation(fileMutationRequest{
		editContext: editContext{ctx: confinedTestCtx(t), files: &failingHistory{}, filetracker: tracker, workingDir: dir},
		call:        fantasy.ToolCall{ID: "call"}, filePath: path, sessionID: "session", toolName: WriteToolName,
		prepare: func(snapshot fileSnapshot) (preparedFileMutation, error) {
			return preparedFileMutation{diffContent: "new", writeContent: "new", wholeFileRead: true, description: "write", permParams: WritePermissionsParams{}, successMessage: "done", metadata: func(string, string, int, int) any { return WriteResponseMetadata{} }}, nil
		},
	})
	require.Error(t, err)
	require.True(t, mutationCommitted(err))
	require.Equal(t, 1, tracker.reads)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "new", string(content))
}

type mutatingPermissionService struct {
	mockPermissionService
	mutate func()
}

func (service *mutatingPermissionService) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	service.mutate()
	return true, nil
}
