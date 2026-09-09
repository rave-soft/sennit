package thread

import "context"

// Handle is a running workspace hosting one delegation's app: for a
// thread, its own isolated instance (its own .sennit data directory,
// database, and agent coordinator) rooted at the thread's git worktree;
// for a task, the parent workspace's own instance.
type Handle interface {
	// ID identifies the handle to its owning [Spawner]; opaque to the
	// manager, which only ever passes it back to [Spawner.Release].
	ID() string
	// Workspace is the domain-facing slice of the App this handle hosts
	// (see [Workspace]).
	Workspace() Workspace
}

// Spawner bootstraps and tears down the isolated workspace backing a
// thread. Implementations differ in how the resulting workspace's
// lifecycle interacts with the rest of the process — the in-process CLI
// case builds one in internal/app/threadspawn, keeping the app-specific
// bootstrap out of this domain package.
// SpawnRequest identifies the isolated workspace to bootstrap and, when
// resuming, the persisted delegation and session ownership pair whose
// abandoned turn may be repaired before new work is dispatched.
type SpawnRequest struct {
	Path         string
	DelegationID string
	SessionID    string
}

type Spawner interface {
	// Spawn bootstraps a workspace for request and returns a handle to it.
	Spawn(ctx context.Context, request SpawnRequest) (Handle, error)
	// Release tears the workspace identified by id (a value previously
	// returned by Handle.ID) down. Idempotent: releasing an unknown or
	// already-released id is a no-op.
	Release(ctx context.Context, id string) error
}
