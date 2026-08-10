package tools

import (
	"context"
	"time"
)

// ThreadCreateArgs mirrors internal/thread.CreateArgs. It is declared here,
// rather than imported, to keep this package free of a dependency on
// internal/thread: internal/thread imports internal/app, which imports
// internal/agent, which imports this package — importing internal/thread
// from here would close that cycle. internal/thread/agenttool.go adapts
// *thread.Manager to the [ThreadManager] interface below, converting
// between the two packages' otherwise-identical types at the seam.
type ThreadCreateArgs struct {
	Name        string
	Goal        string
	BaseBranch  string
	MergePolicy string
}

// ThreadInfo mirrors internal/thread.Thread, for the same reason.
type ThreadInfo struct {
	ID            string
	Name          string
	Goal          string
	BaseBranch    string
	Branch        string
	WorktreePath  string
	SessionID     string
	Status        string
	MergePolicy   string
	ResultSummary string
	Error         string
	CreatedAt     int64
	UpdatedAt     int64
	CompletedAt   int64
}

// ThreadManager is the subset of internal/thread.Manager's API the thread_*
// tools need. The coordinator is only ever given one for the main agent of
// a main (non-thread) workspace; it is nil everywhere else, and the
// thread_* tools are omitted entirely when it is nil.
type ThreadManager interface {
	Create(ctx context.Context, args ThreadCreateArgs) (ThreadInfo, error)
	List(ctx context.Context) ([]ThreadInfo, error)
	Get(ctx context.Context, idOrName string) (ThreadInfo, error)
	Send(ctx context.Context, idOrName, message string) error
	Wait(ctx context.Context, ids []string, timeout time.Duration) error
	Merge(ctx context.Context, idOrName string) error
	Remove(ctx context.Context, idOrName string, force, deleteBranch bool) error
}
