package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// fakeThreadsWorkspace is a minimal workspace.Workspace double for the
// threads subcommands. It embeds the (nil) interface so any method this
// test doesn't care about panics loudly if a command ever reaches for it,
// rather than silently doing nothing -- only the ThreadController surface
// used by threads.go is implemented below.
type fakeThreadsWorkspace struct {
	workspace.Workspace

	supportsThreads bool

	listThreads  []proto.Thread
	listErr      error
	createResult proto.Thread
	createErr    error
	mergeResult  proto.Thread
	mergeErr     error
	removeErr    error

	// Captured call arguments, for tests that assert on what was passed
	// through.
	createReq  proto.CreateThreadRequest
	mergeID    string
	removeID   string
	removeOpts proto.RemoveThreadOptions
}

func (f *fakeThreadsWorkspace) SupportsThreads() bool { return f.supportsThreads }

func (f *fakeThreadsWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	return f.listThreads, f.listErr
}

func (f *fakeThreadsWorkspace) CreateThread(_ context.Context, req proto.CreateThreadRequest) (proto.Thread, error) {
	f.createReq = req
	return f.createResult, f.createErr
}

func (f *fakeThreadsWorkspace) MergeThread(_ context.Context, id string) (proto.Thread, error) {
	f.mergeID = id
	return f.mergeResult, f.mergeErr
}

func (f *fakeThreadsWorkspace) RemoveThread(_ context.Context, id string, opts proto.RemoveThreadOptions) error {
	f.removeID = id
	f.removeOpts = opts
	return f.removeErr
}

// stubAcquireWorkspace points acquireWorkspace at a func that returns ws
// (or fails, if failErr is non-nil) and tracks whether cleanup was called,
// then restores the real acquireWorkspace when the test ends. This is the
// one seam requireThreads goes through, so it's how every threads_test.go
// case avoids setupWorkspaceWithProgressBar's full in-process app.App.
func stubAcquireWorkspace(t *testing.T, ws workspace.Workspace, failErr error) (cleanupCalled *bool) {
	t.Helper()
	orig := acquireWorkspace
	t.Cleanup(func() { acquireWorkspace = orig })

	called := false
	acquireWorkspace = func(*cobra.Command) (workspace.Workspace, func(), error) {
		if failErr != nil {
			return nil, nil, failErr
		}
		return ws, func() { called = true }, nil
	}
	return &called
}

func newThreadsTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	testCmd := &cobra.Command{Use: "threads"}
	testCmd.SetContext(t.Context())
	var stdout bytes.Buffer
	testCmd.SetOut(&stdout)
	return testCmd, &stdout
}

// --- requireThreads ---

func TestRequireThreads_SupportsThreadsFalse_ErrorsAndCleansUp(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: false}
	cleanupCalled := stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	ctx, gotWs, cleanup, err := requireThreads(testCmd, "custom message")

	require.Error(t, err)
	require.EqualError(t, err, "threads: custom message")
	require.Nil(t, ctx)
	require.Nil(t, gotWs)
	require.Nil(t, cleanup)
	require.True(t, *cleanupCalled, "requireThreads must call cleanup before returning the guard error, or the acquired workspace leaks")
}

func TestRequireThreads_AcquireFails_PropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	stubAcquireWorkspace(t, nil, wantErr)

	testCmd, _ := newThreadsTestCmd(t)
	ctx, gotWs, cleanup, err := requireThreads(testCmd, "unused")

	require.ErrorIs(t, err, wantErr)
	require.Nil(t, ctx)
	require.Nil(t, gotWs)
	require.Nil(t, cleanup)
}

func TestRequireThreads_Supported_ReturnsWorkspaceUncleaned(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: true}
	cleanupCalled := stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	ctx, gotWs, cleanup, err := requireThreads(testCmd, "unused")

	require.NoError(t, err)
	require.NotNil(t, ctx)
	require.Same(t, workspace.Workspace(ws), gotWs)
	require.NotNil(t, cleanup)
	require.False(t, *cleanupCalled, "requireThreads must not clean up on the success path -- that's the caller's job via defer")
}

// --- the SupportsThreads guard, pinned once per subcommand ---

func TestRunThreadsList_NotSupported(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: false}
	cleanupCalled := stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	err := runThreadsList(testCmd, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), "this workspace doesn't support threads (not a git repository, or already inside a thread's own workspace)")
	require.True(t, *cleanupCalled)
}

func TestRunThreadsCreate_NotSupported(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: false}
	cleanupCalled := stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	err := runThreadsCreate(testCmd, []string{"my-thread"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "this workspace doesn't support threads")
	require.True(t, *cleanupCalled)
}

func TestRunThreadsMerge_NotSupported(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: false}
	cleanupCalled := stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	err := runThreadsMerge(testCmd, []string{"my-thread"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "this workspace doesn't support threads")
	require.True(t, *cleanupCalled)
}

func TestRunThreadsRemove_NotSupported(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: false}
	cleanupCalled := stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	err := runThreadsRemove(testCmd, []string{"my-thread"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "this workspace doesn't support threads")
	require.True(t, *cleanupCalled)
}

// --- runThreadsList ---

func TestRunThreadsList_JSON(t *testing.T) {
	ws := &fakeThreadsWorkspace{
		supportsThreads: true,
		listThreads: []proto.Thread{
			{ID: "t1", Name: "alpha", Status: "running"},
		},
	}
	stubAcquireWorkspace(t, ws, nil)

	testCmd, stdout := newThreadsTestCmd(t)
	threadsListJSON = true
	t.Cleanup(func() { threadsListJSON = false })

	require.NoError(t, runThreadsList(testCmd, nil))

	var out []proto.Thread
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Len(t, out, 1)
	require.Equal(t, "alpha", out[0].Name)
}

func TestRunThreadsList_Table(t *testing.T) {
	ws := &fakeThreadsWorkspace{
		supportsThreads: true,
		listThreads: []proto.Thread{
			{Name: "alpha", Status: "running", Branch: "thread/alpha", BaseBranch: "main", Goal: "do a thing"},
		},
	}
	stubAcquireWorkspace(t, ws, nil)
	threadsListJSON = false

	testCmd, stdout := newThreadsTestCmd(t)
	require.NoError(t, runThreadsList(testCmd, nil))

	out := stdout.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 2, "expected a header row and one thread row")
	require.Contains(t, lines[0], "NAME")
	require.Contains(t, lines[0], "GOAL")
	require.Contains(t, lines[1], "alpha")
	require.Contains(t, lines[1], "running")
	require.Contains(t, lines[1], "do a thing")
}

func TestRunThreadsList_ListError(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: true, listErr: errors.New("db down")}
	stubAcquireWorkspace(t, ws, nil)
	threadsListJSON = false

	testCmd, _ := newThreadsTestCmd(t)
	err := runThreadsList(testCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "threads: list:")
	require.Contains(t, err.Error(), "db down")
}

// --- runThreadsCreate ---

func TestRunThreadsCreate_Success(t *testing.T) {
	ws := &fakeThreadsWorkspace{
		supportsThreads: true,
		createResult:    proto.Thread{Name: "my-thread", Branch: "thread/my-thread"},
	}
	stubAcquireWorkspace(t, ws, nil)

	testCmd, stdout := newThreadsTestCmd(t)
	testCmd.Flags().StringVar(&threadsCreateGoal, "goal", "", "")
	require.NoError(t, testCmd.Flags().Set("goal", "ship it"))

	require.NoError(t, runThreadsCreate(testCmd, []string{"my-thread"}))

	require.Equal(t, "my-thread", ws.createReq.Name)
	require.Equal(t, "ship it", ws.createReq.Goal)
	require.Contains(t, stdout.String(), `Created thread "my-thread" (branch thread/my-thread)`)
}

func TestRunThreadsCreate_Error(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: true, createErr: errors.New("name taken")}
	stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	err := runThreadsCreate(testCmd, []string{"dup"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "threads: create:")
	require.Contains(t, err.Error(), "name taken")
}

// --- runThreadsMerge ---

func TestRunThreadsMerge_Success(t *testing.T) {
	ws := &fakeThreadsWorkspace{
		supportsThreads: true,
		mergeResult:     proto.Thread{Name: "my-thread", Status: "merged"},
	}
	stubAcquireWorkspace(t, ws, nil)

	testCmd, stdout := newThreadsTestCmd(t)
	require.NoError(t, runThreadsMerge(testCmd, []string{"my-thread"}))

	require.Equal(t, "my-thread", ws.mergeID)
	require.Contains(t, stdout.String(), `Thread "my-thread": merged`)
}

func TestRunThreadsMerge_Error(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: true, mergeErr: errors.New("conflict")}
	stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	err := runThreadsMerge(testCmd, []string{"my-thread"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "threads: merge:")
	require.Contains(t, err.Error(), "conflict")
}

// --- runThreadsRemove ---

func TestRunThreadsRemove_Success(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: true}
	stubAcquireWorkspace(t, ws, nil)

	testCmd, stdout := newThreadsTestCmd(t)
	testCmd.Flags().BoolVar(&threadsRemoveForce, "force", false, "")
	testCmd.Flags().BoolVar(&threadsRemoveDeleteBranch, "delete-branch", false, "")
	require.NoError(t, testCmd.Flags().Set("force", "true"))
	require.NoError(t, testCmd.Flags().Set("delete-branch", "true"))
	t.Cleanup(func() { threadsRemoveForce, threadsRemoveDeleteBranch = false, false })

	require.NoError(t, runThreadsRemove(testCmd, []string{"my-thread"}))

	require.Equal(t, "my-thread", ws.removeID)
	require.True(t, ws.removeOpts.Force)
	require.True(t, ws.removeOpts.DeleteBranch)
	require.Contains(t, stdout.String(), `Removed thread "my-thread"`)
}

func TestRunThreadsRemove_Error(t *testing.T) {
	ws := &fakeThreadsWorkspace{supportsThreads: true, removeErr: errors.New("dirty worktree")}
	stubAcquireWorkspace(t, ws, nil)

	testCmd, _ := newThreadsTestCmd(t)
	err := runThreadsRemove(testCmd, []string{"my-thread"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "threads: remove:")
	require.Contains(t, err.Error(), "dirty worktree")
}

// --- renderThreadsTable ---

func TestRenderThreadsTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderThreadsTable(&buf, nil))
	require.Equal(t, "No threads.\n", buf.String())
}

func TestRenderThreadsTable_OneRow(t *testing.T) {
	var buf bytes.Buffer
	threads := []proto.Thread{
		{Name: "alpha", Status: "running", Branch: "thread/alpha", BaseBranch: "main", Goal: "do a thing"},
	}
	require.NoError(t, renderThreadsTable(&buf, threads))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	require.Equal(t, "NAME\tSTATUS\tBRANCH\tBASE\tUPDATED\tGOAL", stripTabAlign(lines[0]))
	require.Contains(t, lines[1], "alpha")
	require.Contains(t, lines[1], "running")
	require.Contains(t, lines[1], "thread/alpha")
	require.Contains(t, lines[1], "main")
	require.Contains(t, lines[1], "do a thing")
}

// stripTabAlign undoes tabwriter's column padding so a rendered header can
// be compared against the literal header string runThreadsList/
// renderThreadsTable writes.
func stripTabAlign(line string) string {
	fields := strings.Fields(line)
	return strings.Join(fields, "\t")
}

func TestRenderThreadsTable_GoalTruncation_Boundary(t *testing.T) {
	// Exactly 60 runes must survive untouched.
	goal60 := strings.Repeat("g", 60)
	var buf bytes.Buffer
	require.NoError(t, renderThreadsTable(&buf, []proto.Thread{{Name: "a", Goal: goal60}}))
	require.Contains(t, buf.String(), goal60)
	require.NotContains(t, buf.String(), "…")
}

func TestRenderThreadsTable_GoalTruncation_OverBoundary(t *testing.T) {
	// 61 characters must be cut to 59 plus the ellipsis marker.
	goal61 := strings.Repeat("g", 61)
	var buf bytes.Buffer
	require.NoError(t, renderThreadsTable(&buf, []proto.Thread{{Name: "a", Goal: goal61}}))

	out := buf.String()
	require.Contains(t, out, strings.Repeat("g", 59)+"…")
	require.NotContains(t, out, goal61, "the untruncated 61-character goal must not appear verbatim")
}
