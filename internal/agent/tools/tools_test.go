package tools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// stubPermissionService lets tests control what Request returns, unlike the
// always-grant mocks used elsewhere in this package.
type stubPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
	granted bool
	err     error
}

func (s *stubPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return s.granted, s.err
}

func (s *stubPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (s *stubPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (s *stubPermissionService) GrantPersistent(req permission.PermissionRequest) bool { return true }
func (s *stubPermissionService) AutoApproveSession(sessionID string)                   {}
func (s *stubPermissionService) SetSkipRequests(skip bool)                             {}
func (s *stubPermissionService) SkipRequests() bool                                    { return false }

func (s *stubPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

func TestRequirePermissionGranted(t *testing.T) {
	perms := &stubPermissionService{granted: true}

	resp, denied, err := requirePermission(context.Background(), perms, permission.CreatePermissionRequest{})
	require.NoError(t, err)
	require.False(t, denied)
	require.Zero(t, resp)
}

func TestRequirePermissionDenied(t *testing.T) {
	perms := &stubPermissionService{granted: false}

	resp, denied, err := requirePermission(context.Background(), perms, permission.CreatePermissionRequest{})
	require.NoError(t, err)
	require.True(t, denied)
	require.True(t, resp.IsError)
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "User denied permission")
}

func TestRequirePermissionServiceError(t *testing.T) {
	wantErr := errors.New("boom")
	perms := &stubPermissionService{err: wantErr}

	_, denied, err := requirePermission(context.Background(), perms, permission.CreatePermissionRequest{})
	require.ErrorIs(t, err, wantErr)
	require.False(t, denied)
}

func TestResolveWithinWorkdir(t *testing.T) {
	workingDir := t.TempDir()

	absPath, outside, err := resolveWithinWorkdir(workingDir, filepath.Join(workingDir, "sub", "file.txt"))
	require.NoError(t, err)
	require.False(t, outside)
	require.True(t, filepath.IsAbs(absPath))

	absPath, outside, err = resolveWithinWorkdir(workingDir, filepath.Join(workingDir, "..", "elsewhere.txt"))
	require.NoError(t, err)
	require.True(t, outside)
	require.True(t, filepath.IsAbs(absPath))

	// A sibling name that merely starts with ".." (e.g. "..foo") must not
	// be mistaken for an escape via "..": a bare
	// strings.HasPrefix(relPath, "..") check (the old bug) flags it as
	// outside workingDir even though it resolves to a file inside it.
	absPath, outside, err = resolveWithinWorkdir(workingDir, filepath.Join(workingDir, "..foo"))
	require.NoError(t, err)
	require.False(t, outside, "..foo is a sibling name inside workingDir, not an escape")
	require.True(t, filepath.IsAbs(absPath))
}

func TestEnsureParentDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "file.txt")

	require.NoError(t, ensureParentDir(target))

	info, err := os.Stat(filepath.Join(dir, "a", "b"))
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestWriteFileWithHistoryCreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "new.txt")
	files := &mockHistoryService{}

	err := writeFileWithHistory(context.Background(), files, "session", filePath, "", "hello")
	require.NoError(t, err)

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "hello", string(content))
}

func TestCommandAvailable(t *testing.T) {
	require.True(t, commandAvailable(func(string) (string, error) { return "/tools/gh", nil }, "gh"))
	require.False(t, commandAvailable(func(string) (string, error) { return "", os.ErrNotExist }, "gh"))
}

func TestToolAvailabilityIsPerInstance(t *testing.T) {
	unavailable := NewFetchTool(nil, t.TempDir(), nil, withGHAvailability(false))
	available := NewFetchTool(nil, t.TempDir(), nil, withGHAvailability(true))

	require.NotContains(t, unavailable.Info().Description, "use `gh` CLI")
	require.Contains(t, available.Info().Description, "use `gh` CLI")
}

func TestNewHTTPClientAppliesTimeout(t *testing.T) {
	client := newHTTPClient(5 * time.Second)
	require.Equal(t, 5*time.Second, client.Timeout)

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, 100, transport.MaxIdleConns)
	require.Equal(t, 10, transport.MaxIdleConnsPerHost)
	require.Equal(t, 90*time.Second, transport.IdleConnTimeout)
}

// TestInvalidParamFormat pins invalidParam's wording so the ~40 "X is
// required" call sites it replaced stay consistent instead of drifting back
// to per-tool phrasing.
func TestInvalidParamFormat(t *testing.T) {
	resp := invalidParam("file_path")
	require.True(t, resp.IsError)
	require.Equal(t, "file_path is required", resp.Content)
}

// TestMissingSessionIDFormat pins missingSessionID's wording, the error-side
// counterpart to invalidParam for the "session ID absent from context" case.
func TestMissingSessionIDFormat(t *testing.T) {
	err := missingSessionID("editing a file")
	require.EqualError(t, err, "session ID is required for editing a file")
}

func (s *stubPermissionService) ActiveRequest() (permission.PermissionRequest, bool) {
	return permission.PermissionRequest{}, false
}

func (*stubPermissionService) ConfineToWorkingDir() {}
func (*stubPermissionService) ConfinedDir() string  { return "" }
