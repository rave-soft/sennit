package tools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/pubsub"
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
	tracker := mockFileTrackerService{}

	err := writeFileWithHistory(context.Background(), files, tracker, "session", filePath, "", "hello")
	require.NoError(t, err)

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "hello", string(content))
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

func (s *stubPermissionService) ActiveRequest() (permission.PermissionRequest, bool) {
	return permission.PermissionRequest{}, false
}
