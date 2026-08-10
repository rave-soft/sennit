package strand

// EventType identifies the kind of lifecycle change a [Manager] publishes
// through [Manager.Subscribe].
type EventType string

const (
	// EventCreated fires once a strand row exists (before its worktree
	// and workspace are set up), so subscribers see it immediately.
	EventCreated EventType = "created"
	// EventStatusChanged fires on every status transition, including
	// the ones EventMerged and EventRemoved also fire for.
	EventStatusChanged EventType = "status_changed"
	// EventMerged fires once a strand's branch has landed in its base
	// branch.
	EventMerged EventType = "merged"
	// EventRemoved fires once a strand's worktree, branch (if
	// requested), and store row have all been cleaned up.
	EventRemoved EventType = "removed"
)

// Event is published by [Manager] on every strand lifecycle change. Strand
// carries the row as it stood at the time of the event.
type Event struct {
	Type   EventType
	Strand Strand
}
