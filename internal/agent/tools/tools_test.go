package tools

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/history"
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

// TestResolveWithinWorkdir_DirectorySymlinkEscape pins DEFECT 1: a directory
// symlink lexically inside workingDir (as "ln -s ../.. up" would create with
// the bash tool) must not defeat the boundary check just because the plain
// filepath.Abs form of "up/outside.txt" looks like it is inside workingDir.
func TestResolveWithinWorkdir_DirectorySymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workingDir := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(workingDir, 0o755))

	// "up" -> "../..", so workingDir/up resolves to root's parent: outside
	// workingDir even though "workingDir/up/outside.txt" is lexically inside
	// it.
	require.NoError(t, os.Symlink(filepath.Join("..", ".."), filepath.Join(workingDir, "up")))

	_, outside, err := resolveWithinWorkdir(workingDir, filepath.Join(workingDir, "up", "outside.txt"))
	require.NoError(t, err)
	require.True(t, outside, "a directory symlink must not defeat the workingDir boundary")
}

// TestResolveWithinWorkdir_WorkdirReachedThroughSymlink pins the other half
// of DEFECT 1's fix: resolving symlinks in the boundary check must not make
// a workingDir that is itself reached through a symlink (a common macOS
// /tmp situation) report its own children as outside.
func TestResolveWithinWorkdir_WorkdirReachedThroughSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	linked := filepath.Join(root, "linked")
	require.NoError(t, os.Symlink(real, linked))

	_, outside, err := resolveWithinWorkdir(linked, filepath.Join(linked, "sub", "file.txt"))
	require.NoError(t, err)
	require.False(t, outside)
}

// TestResolveWithinWorkdir_DanglingSymlinkEscape pins the residual hole in
// DEFECT 1's first fix: a symlink whose target does not exist ("up" ->
// "../../evil", where "evil" is absent) makes EvalSymlinks fail with
// ENOENT exactly like a path component that simply hasn't been created
// yet. Folding the two together let "up/foo.txt" resolve as an
// as-yet-nonexistent ordinary path and report as inside workingDir, even
// though the write's own MkdirAll would follow the dangling link out.
func TestResolveWithinWorkdir_DanglingSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workingDir := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(workingDir, 0o755))

	require.NoError(t, os.Symlink(filepath.Join("..", "..", "evil"), filepath.Join(workingDir, "up")))

	_, outside, err := resolveWithinWorkdir(workingDir, filepath.Join(workingDir, "up", "foo.txt"))
	require.NoError(t, err)
	require.True(t, outside, "a dangling symlink must not be treated as a not-yet-existing path component")
}

// TestResolveWithinWorkdir_GenuinelyMissingNestedPath makes sure the
// dangling-symlink fix above does not overshoot: an ordinary nested path
// with no symlink anywhere in it, none of whose components exist yet, must
// still resolve as inside workingDir — that is the common case a write
// tool relies on.
func TestResolveWithinWorkdir_GenuinelyMissingNestedPath(t *testing.T) {
	workingDir := t.TempDir()

	_, outside, err := resolveWithinWorkdir(workingDir, filepath.Join(workingDir, "newdir", "sub", "file.txt"))
	require.NoError(t, err)
	require.False(t, outside)
}

func TestEnsureParentDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "file.txt")

	require.NoError(t, ensureParentDir(target))

	info, err := os.Stat(filepath.Join(dir, "a", "b"))
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

// recordingHistoryService records what was versioned, in order, and can be
// told whether the path already has history and what its stored content is.
type recordingHistoryService struct {
	*mockHistoryService
	stored   string
	hasEntry bool
	versions []string
}

func (r *recordingHistoryService) GetByPathAndSession(ctx context.Context, path, sessionID string) (history.File, error) {
	if !r.hasEntry {
		return history.File{}, sql.ErrNoRows
	}
	return history.File{Path: path, Content: r.stored}, nil
}

func (r *recordingHistoryService) CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error) {
	r.versions = append(r.versions, content)
	return history.File{}, nil
}

// TestRecordFileHistory covers the three shapes of a committed write. It
// replaces a test of a combined write-and-record helper that no tool used:
// that helper wrote with os.WriteFile, which is exactly the check
// applyFileMutation exists to keep, and the test asserted only that the
// bytes reached the disk — nothing about the history it was named for.
func TestRecordFileHistory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		hasEntry bool
		stored   string
		old      string
		want     []string
	}{
		{
			// Nothing recorded yet: what was on disk becomes the
			// baseline, then the new content.
			name: "first write in this session",
			old:  "before",
			want: []string{"before", "after"},
		},
		{
			// History agrees with what was on disk, so no intermediate
			// version is needed.
			name:     "history already matches disk",
			hasEntry: true,
			stored:   "before",
			old:      "before",
			want:     []string{"after"},
		},
		{
			// Someone edited the file outside Sennit between two tool
			// writes; that content would be lost on undo without a
			// version of its own.
			name:     "changed on disk behind us",
			hasEntry: true,
			stored:   "recorded",
			old:      "edited by hand",
			want:     []string{"edited by hand", "after"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := &recordingHistoryService{hasEntry: tc.hasEntry, stored: tc.stored}
			require.NoError(t, recordFileHistory(context.Background(), files, "session", "/w/file.txt", tc.old, "after"))
			require.Equal(t, tc.want, files.versions)
		})
	}
}

// TestRecordFileHistory_NoSessionIsNotAnError: a tool call outside a
// session has nowhere to record to, and that is not a failure to report.
func TestRecordFileHistory_NoSessionIsNotAnError(t *testing.T) {
	t.Parallel()

	files := &recordingHistoryService{}
	require.NoError(t, recordFileHistory(context.Background(), files, "", "/w/file.txt", "", "after"))
	require.Empty(t, files.versions)
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

func (s *stubPermissionService) AwaitingAnswer(string) bool { return false }

func (*stubPermissionService) ConfineToWorkingDir() {}
func (*stubPermissionService) ConfinedDir() string  { return "" }
