package tools

import (
	"context"
	"errors"
	"fmt"
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
	Name            string
	Goal            string
	BaseBranch      string
	MergePolicy     string
	ParentSessionID string
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
// ErrThreadNotFound reports that no thread matches the id or name given.
// Callers should say more than "not found" when they surface it: a thread
// is deleted once it merges, so a name that resolved a minute ago
// resolving to nothing now usually means the work landed, not that the
// caller got the name wrong.
var ErrThreadNotFound = errors.New("no such thread")

// SendOutcome mirrors internal/thread.SendDisposition, for the same
// import-cycle reason as the types above: what actually became of a
// message handed to thread_send/task_send. A send is never silently
// dropped, but it can land in the target session's prompt queue behind a
// turn already in flight — for an agent inside a long sub-agent call, that
// can be minutes away — and the tools report that difference back to the
// model rather than answering "sent" either way. See [SendOutcome.Describe].
type SendOutcome struct {
	// Queued is true when the target was mid-turn, so the message becomes
	// its next prompt instead of its current one.
	Queued bool
	// Ahead is how many prompts were already waiting ahead of this one.
	// Only meaningful when Queued is true.
	Ahead int
	// Resumed is true when the delegation's workspace had to be respawned
	// to take the message.
	Resumed bool
}

// Describe renders the outcome as the sentence the send tools return. kind
// is the delegation kind as it should read in prose ("thread", "task") and
// idOrName is how the caller addressed it.
//
// The queued wording deliberately spells out the consequence rather than
// only the state: a model that reads "queued" alone tends to keep treating
// the message as delivered, and the whole point of reporting this is that a
// deadline or a course correction sitting behind a long turn has not
// reached anyone yet.
func (o SendOutcome) Describe(kind, idOrName string) string {
	switch {
	case o.Queued && o.Ahead > 0:
		return fmt.Sprintf(
			"Queued message for %s %q. Its agent is mid-turn, and %d earlier message(s) are already waiting, so this one is read only after those turns finish — it cannot steer the work in flight.",
			kind, idOrName, o.Ahead,
		)
	case o.Queued:
		return fmt.Sprintf(
			"Queued message for %s %q. Its agent is mid-turn, so this message is read only when the current turn finishes — it cannot steer the work in flight.",
			kind, idOrName,
		)
	case o.Resumed:
		return fmt.Sprintf("Resumed %s %q and delivered the message as its next turn.", kind, idOrName)
	default:
		return fmt.Sprintf("Delivered message to idle %s %q; it starts a turn on it now.", kind, idOrName)
	}
}

type ThreadManager interface {
	Create(ctx context.Context, args ThreadCreateArgs) (ThreadInfo, error)
	List(ctx context.Context) ([]ThreadInfo, error)
	// Get resolves a thread by id or name. It must report
	// [ErrThreadNotFound] for an id that resolves to something which is
	// not a thread: internal/thread keeps both kinds in one table, and
	// delegationView.lookup treats a hit here as proof the id is a
	// thread's - a task returned from here would be reported to a caller
	// the task scoping means to keep it from.
	Get(ctx context.Context, idOrName string) (ThreadInfo, error)
	// Send hands message to the thread's session and reports whether its
	// agent picks it up now or only after the turn it is already running.
	Send(ctx context.Context, idOrName, message string) (SendOutcome, error)
	// Cancel stops the thread's in-flight run, recording reason as its
	// terminal error, for agent_cancel. The worktree and branch survive:
	// a cancelled thread's work can still be read or resumed, and
	// clearing it away is thread_remove's job.
	Cancel(ctx context.Context, idOrName, reason string) error
	Wait(ctx context.Context, ids []string, timeout time.Duration) error
	// Merge returns the thread as the attempt left it. A clean merge
	// discards the thread, so there is nothing left to Get afterwards —
	// this return value is the only report of the outcome.
	Merge(ctx context.Context, idOrName string) (ThreadInfo, error)
	Remove(ctx context.Context, idOrName string, force, deleteBranch bool) error
}
