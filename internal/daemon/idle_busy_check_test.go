// This file tests idleBusyCheck.busy directly, white-box (package daemon,
// not daemon_test): the end-to-end tests in idle_test.go drive a real
// daemon.Run and observe wall-clock exit timing, but grpc.Server.
// GracefulStop itself keeps a Run call from returning for as long as a
// connected client's Subscribe stream is open, REGARDLESS of what
// idleBusyCheck.busy reports. Discovered while hand-verifying (per
// AGENTS.md's "a test you have not seen fail is not evidence") that
// deleting the ClientCount check actually turns something red: the
// end-to-end client test kept passing anyway, for the wrong reason
// (GracefulStop draining the still-open stream, not the busy check
// itself), until the observation window happened to be widened past that
// drain time. A direct, sub-millisecond unit test of busy() against a
// fake workspace.Workspace and a fake client counter is what actually
// pins each source down independent of that confound.
package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// fakeClientCounter is a settable idleClientCounter.
type fakeClientCounter int

func (f fakeClientCounter) ClientCount() int { return int(f) }

// idleTestWorkspace wraps wsrpctest.StubWorkspace to add the two methods
// it doesn't stub itself (ListThreads/ListTasks), since idleBusyCheck.busy
// needs both.
type idleTestWorkspace struct {
	*wsrpctest.StubWorkspace
	listThreadsResult []proto.Thread
	listThreadsErr    error
	listTasksResult   []proto.Thread
	listTasksErr      error
	// children, when non-nil, makes this workspace a worktreeAggregator
	// (see idle.go) -- letting a test put a registered worktree workspace
	// in the busy check's path the same way *appws.AppWorkspace's root
	// instance would.
	children []workspace.Workspace
}

// WorktreeChildren makes idleTestWorkspace satisfy worktreeAggregator
// whenever children is set; an idleTestWorkspace with no children set
// still reports the zero value (nil), same as a workspace that was never
// asked to be an aggregator at all -- busy's type assertion still
// succeeds, it simply has nothing to range over.
func (w *idleTestWorkspace) WorktreeChildren() []workspace.Workspace {
	return w.children
}

func newIdleTestWorkspace() *idleTestWorkspace {
	return &idleTestWorkspace{StubWorkspace: &wsrpctest.StubWorkspace{}}
}

func (w *idleTestWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	return w.listThreadsResult, w.listThreadsErr
}

func (w *idleTestWorkspace) ListTasks(context.Context) ([]proto.Thread, error) {
	return w.listTasksResult, w.listTasksErr
}

// idleTestCase is one busy() source, tested in isolation against an
// otherwise entirely-idle workspace/client-counter pair.
type idleTestCase struct {
	name     string
	clients  idleClientCounter
	ws       func() workspace.Workspace
	wantBusy bool
}

func TestIdleBusyCheck(t *testing.T) {
	idleWS := func() workspace.Workspace { return newIdleTestWorkspace() }

	for _, tc := range []idleTestCase{
		{
			name:    "nothing running",
			clients: fakeClientCounter(0),
			ws:      idleWS,
		},
		{
			name:     "a connected client",
			clients:  fakeClientCounter(1),
			ws:       idleWS,
			wantBusy: true,
		},
		{
			name:    "a busy session",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.AgentActivityResult = workspace.AgentActivity{BusySessions: []string{"sess-1"}}
				return ws
			},
			wantBusy: true,
		},
		{
			name:    "an active background shell",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.BackgroundJobCountsResult = workspace.BackgroundJobCounts{Active: 1}
				return ws
			},
			wantBusy: true,
		},
		{
			name:    "a non-terminal thread",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.SupportsThreadsResult = true
				ws.listThreadsResult = []proto.Thread{{ID: "t1", Status: string(proto.ThreadStatusRunning)}}
				return ws
			},
			wantBusy: true,
		},
		{
			name:    "only terminal threads",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.SupportsThreadsResult = true
				ws.listThreadsResult = []proto.Thread{{ID: "t1", Status: string(proto.ThreadStatusCompleted)}}
				return ws
			},
		},
		{
			name:    "ListThreads erroring is treated as busy",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.SupportsThreadsResult = true
				ws.listThreadsErr = errors.New("boom")
				return ws
			},
			wantBusy: true,
		},
		{
			name:    "a non-terminal task",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.SupportsTasksResult = true
				ws.listTasksResult = []proto.Thread{{ID: "task-1", Status: string(proto.ThreadStatusPending)}}
				return ws
			},
			wantBusy: true,
		},
		{
			name:    "only terminal tasks",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.SupportsTasksResult = true
				ws.listTasksResult = []proto.Thread{{ID: "task-1", Status: string(proto.ThreadStatusFailed)}}
				return ws
			},
		},
		{
			name:    "ListTasks erroring is treated as busy",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.SupportsTasksResult = true
				ws.listTasksErr = errors.New("boom")
				return ws
			},
			wantBusy: true,
		},
		{
			name:    "a pending permission request",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.PendingPromptsResult = workspace.PendingPrompts{
					Permissions: []permission.PermissionRequest{{ID: "p1"}},
				}
				return ws
			},
			wantBusy: true,
		},
		{
			// Review point 4: a pending question must hold the daemon
			// open exactly like a pending permission, including a
			// delegation's own question (PendingPrompts already gathers
			// those -- see workspace.PendingPrompts' doc comment).
			name:    "a pending question",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.PendingPromptsResult = workspace.PendingPrompts{
					Questions: []question.Request{{ID: "q1"}},
				}
				return ws
			},
			wantBusy: true,
		},
		{
			name:    "PendingPrompts erroring is treated as busy",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				ws := newIdleTestWorkspace()
				ws.PendingPromptsErr = errors.New("boom")
				return ws
			},
			wantBusy: true,
		},
		{
			// CLIENT-SERVER.md PR 2.4b: a turn running in an orphaned
			// worktree App (no client attached) must count as busy even
			// though the root workspace's own AgentActivity reports
			// nothing.
			name:    "a busy session in a registered worktree workspace",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				root := newIdleTestWorkspace()
				child := newIdleTestWorkspace()
				child.AgentActivityResult = workspace.AgentActivity{BusySessions: []string{"sess-1"}}
				root.children = []workspace.Workspace{child}
				return root
			},
			wantBusy: true,
		},
		{
			name:    "a pending permission in a registered worktree workspace",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				root := newIdleTestWorkspace()
				child := newIdleTestWorkspace()
				child.PendingPromptsResult = workspace.PendingPrompts{
					Permissions: []permission.PermissionRequest{{ID: "p1"}},
				}
				root.children = []workspace.Workspace{child}
				return root
			},
			wantBusy: true,
		},
		{
			name:    "an idle registered worktree workspace does not count as busy",
			clients: fakeClientCounter(0),
			ws: func() workspace.Workspace {
				root := newIdleTestWorkspace()
				root.children = []workspace.Workspace{newIdleTestWorkspace()}
				return root
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := &idleBusyCheck{ws: tc.ws(), clients: tc.clients}
			busy, reason := check.busy(context.Background())
			require.Equal(t, tc.wantBusy, busy, "reason: %q", reason)
			if tc.wantBusy {
				require.NotEmpty(t, reason)
			}
		})
	}
}
