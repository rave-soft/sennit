// Package gc implements the collection logic behind `sennit gc`: which
// sessions and threads are old enough to reclaim, and deleting them. It is
// deliberately free of cobra/CLI concerns (flag parsing, config loading,
// text/JSON rendering) so the selection and deletion rules can be tested
// directly; internal/cmd wires it to the CLI.
package gc

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	sennitdb "github.com/rave-soft/sennit/internal/db"
)

// Deps are the database handles Run needs. Queries and Conn must be bound
// to the same underlying database.
type Deps struct {
	Queries *sennitdb.Queries
	Conn    *sql.DB
}

// Policy controls what a single Run call collects and, unless DryRun,
// deletes.
type Policy struct {
	// Cutoff is the unix timestamp: a session or thread with updated_at
	// strictly before this is eligible for collection.
	Cutoff int64
	// ProjectPath scopes collection to sessions/threads under this path.
	// Empty means every project sharing the database.
	ProjectPath string
	// DryRun collects and reports without deleting anything or running
	// VACUUM.
	DryRun bool
}

// Report summarizes what one Run call collected (DryRun) or deleted.
type Report struct {
	SessionsDeleted  int
	MessagesDeleted  int64
	FilesDeleted     int64
	ReadFilesDeleted int64
	ThreadsDeleted   int

	// OrphanedWorktrees lists the git worktrees a deleted (or, in a dry
	// run, about-to-be-deleted) thread owned that Run found on disk. Run
	// reports these but never removes them -- see selectThreads. This is
	// a deliberate scope decision: a worktree may hold uncommitted user
	// work, and deleting it is the user's call, not gc's.
	OrphanedWorktrees []string

	// Vacuumed reports whether Run issued VACUUM. It runs only after a
	// non-dry-run that actually deleted something -- an empty selection
	// leaves nothing to reclaim, and DryRun never writes at all.
	Vacuumed bool
}

// Selection is the set of rows Run has decided to reclaim, together with
// the counts of dependent rows they carry. It is the argument DeleteFunc
// receives and the basis both Collect (dry run) and DeleteWith
// (authoritative) build a Report from.
type Selection struct {
	SessionIDs        []string
	ThreadIDs         []string
	MessagesDeleted   int64
	FilesDeleted      int64
	ReadFilesDeleted  int64
	OrphanedWorktrees []string
}

func (s Selection) report() Report {
	return Report{
		SessionsDeleted:   len(s.SessionIDs),
		MessagesDeleted:   s.MessagesDeleted,
		FilesDeleted:      s.FilesDeleted,
		ReadFilesDeleted:  s.ReadFilesDeleted,
		ThreadsDeleted:    len(s.ThreadIDs),
		OrphanedWorktrees: s.OrphanedWorktrees,
	}
}

// Run collects sessions and threads older than policy.Cutoff (scoped to
// policy.ProjectPath when set) and, unless policy.DryRun, deletes them and
// reclaims the freed space with VACUUM.
func Run(ctx context.Context, deps Deps, policy Policy) (Report, error) {
	if policy.DryRun {
		selection, err := Collect(ctx, deps.Queries, policy.Cutoff, policy.ProjectPath)
		if err != nil {
			return Report{}, fmt.Errorf("selecting history for gc: %w", err)
		}
		return selection.report(), nil
	}

	selection, err := Delete(ctx, deps.Conn, deps.Queries, policy.Cutoff, policy.ProjectPath)
	if err != nil {
		return Report{}, fmt.Errorf("deleting gc'd history: %w", err)
	}
	report := selection.report()
	if len(selection.SessionIDs) > 0 || len(selection.ThreadIDs) > 0 {
		if err := Vacuum(ctx, deps.Conn); err != nil {
			return Report{}, fmt.Errorf("reclaiming database space: %w", err)
		}
		report.Vacuumed = true
	}
	return report, nil
}

// Collect selects history and counts its dependent rows using q's current
// snapshot. Authoritative callers pass a *Queries bound to the writer
// transaction; dry runs pass one bound to the plain database handle.
func Collect(ctx context.Context, q *sennitdb.Queries, cutoff int64, projectPath string) (Selection, error) {
	sessionIDs, err := selectSessions(ctx, q, cutoff, projectPath)
	if err != nil {
		return Selection{}, err
	}
	threadIDs, orphanedWorktrees, err := selectThreads(ctx, q, cutoff, projectPath)
	if err != nil {
		return Selection{}, err
	}

	selection := Selection{SessionIDs: sessionIDs, ThreadIDs: threadIDs, OrphanedWorktrees: orphanedWorktrees}
	selection.MessagesDeleted, selection.FilesDeleted, selection.ReadFilesDeleted, err = countDependents(ctx, q, sessionIDs)
	if err != nil {
		return Selection{}, err
	}
	return selection, nil
}

// countDependents totals the messages/files/read_files rows belonging to
// sessionIDs with one aggregate COUNT query per table, via the generated
// Count*ForSessionIDs queries (json_each over a single JSON-array
// parameter -- see internal/db/sql/messages.sql). That keeps this to
// three queries total regardless of how many sessions were selected, not
// three per session, which inside the writer transaction would hold the
// database's write lock across many reads before the first delete.
func countDependents(ctx context.Context, q *sennitdb.Queries, sessionIDs []string) (messages, files, readFiles int64, err error) {
	if len(sessionIDs) == 0 {
		return 0, 0, 0, nil
	}
	idsJSON, err := json.Marshal(sessionIDs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("marshaling session ids for gc: %w", err)
	}
	messages, err = q.CountMessagesForSessionIDs(ctx, string(idsJSON))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("counting messages for gc: %w", err)
	}
	files, err = q.CountFilesForSessionIDs(ctx, string(idsJSON))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("counting files for gc: %w", err)
	}
	readFiles, err = q.CountReadFilesForSessionIDs(ctx, string(idsJSON))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("counting read_files for gc: %w", err)
	}
	return messages, files, readFiles, nil
}

// protectedSessions returns the set of session IDs that must never be
// swept: threads.session_id and threads.parent_session_id (one hop, non-
// empty only) of every thread whose status is non-terminal. This must
// agree with selectThreads's own terminal() check -- the bug this fixes
// is that selectSessions used to decide deletions purely from session age
// and sessions.parent_session_id descendant expansion, never consulting
// the threads table, so a live delegation's session (or the parent
// session its completion delivers into, see internal/thread/manager.go
// and internal/agent/dispatch.go) could be swept out from under it and
// violate messages.session_id's foreign key. Deliberately unscoped by
// projectPath: it reads every thread so a same-project non-terminal
// thread's session is never dropped by a project filter that belongs to
// selection, not to protection; a protected ID outside the scope being
// collected is simply never a candidate for selected in the first place.
func protectedSessions(rows []sennitdb.ListThreadsForGCRow) map[string]bool {
	protected := make(map[string]bool)
	for _, r := range rows {
		if persistedThreadStatus(r.Status).terminal() {
			continue
		}
		if r.SessionID != "" {
			protected[r.SessionID] = true
		}
		if r.ParentSessionID != "" {
			protected[r.ParentSessionID] = true
		}
	}
	return protected
}

// selectSessions returns the IDs of every session gc should delete: every
// session whose updated_at is strictly older than cutoff (scoped to
// projectPath when non-empty), expanded to include any agent-tool/title
// sub-session parented to a selected session, regardless of the
// sub-session's own age. The expansion runs to a fixed point so a
// sub-session's own sub-sessions are swept up too. A session named by
// protectedSessions is excluded from both the initial age-based pass and
// the descendant expansion -- since it is never added to selected, the
// BFS never traverses through it either, so nothing beneath it is force-
// swept via that path (anything beneath it that is independently old and
// unprotected is still swept on its own merits, same as today).
func selectSessions(ctx context.Context, q *sennitdb.Queries, cutoff int64, projectPath string) ([]string, error) {
	rows, err := q.ListSessionsForGC(ctx)
	if err != nil {
		return nil, err
	}
	threadRows, err := q.ListThreadsForGC(ctx)
	if err != nil {
		return nil, err
	}
	protected := protectedSessions(threadRows)

	childrenByParent := make(map[string][]string)
	inScope := make(map[string]bool)
	for _, r := range rows {
		if projectPath != "" && r.ProjectPath != projectPath {
			continue
		}
		inScope[r.ID] = true
		if r.ParentSessionID.Valid && r.ParentSessionID.String != "" {
			childrenByParent[r.ParentSessionID.String] = append(childrenByParent[r.ParentSessionID.String], r.ID)
		}
	}

	selected := make(map[string]bool)
	var queue []string
	for _, r := range rows {
		if !inScope[r.ID] || protected[r.ID] {
			continue
		}
		if r.UpdatedAt < cutoff && !selected[r.ID] {
			selected[r.ID] = true
			queue = append(queue, r.ID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, childID := range childrenByParent[id] {
			if protected[childID] {
				continue
			}
			if !selected[childID] {
				selected[childID] = true
				queue = append(queue, childID)
			}
		}
	}

	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// persistedThreadStatus is a status value as it is persisted in the
// threads table. gc keeps its own copy of the terminal set instead of
// reaching for the domain (internal/thread) or the wire DTOs (internal/proto):
// the collector only needs to know whether a row is finished, and must not
// depend on either boundary for it. TestTerminalStatusParityWithThread pins
// the set against the domain's, so a status a newer thread package calls
// terminal (or live) can never silently change what `sennit gc` deletes.
type persistedThreadStatus string

// persistedThreadKind is a kind value as it is persisted in the threads
// table; gc only distinguishes the kind that owns a git worktree.
type persistedThreadKind string

const (
	persistedStatusCompleted    persistedThreadStatus = "completed"
	persistedStatusMerged       persistedThreadStatus = "merged"
	persistedStatusFailed       persistedThreadStatus = "failed"
	persistedStatusCancelled    persistedThreadStatus = "cancelled"
	persistedStatusConflict     persistedThreadStatus = "conflict"
	persistedStatusMergeBlocked persistedThreadStatus = "merge_blocked"
	persistedStatusInterrupted  persistedThreadStatus = "interrupted"
	persistedKindThread         persistedThreadKind   = "thread"
)

// terminal reports whether status is a known terminal status. Unknown
// statuses are neither active nor terminal and are deliberately retained
// for forward compatibility: a status from a newer version sharing the
// database is not finished as far as this binary knows.
func (status persistedThreadStatus) terminal() bool {
	switch status {
	case persistedStatusCompleted,
		persistedStatusMerged,
		persistedStatusFailed,
		persistedStatusCancelled,
		persistedStatusConflict,
		persistedStatusMergeBlocked,
		persistedStatusInterrupted:
		return true
	default:
		return false
	}
}

// selectThreads returns the IDs of finished delegations (never
// pending/running/merging) whose updated_at is strictly older than
// cutoff, scoped to projectPath when non-empty, plus the worktree paths
// gc is about to orphan by deleting their owning thread row.
//
// Both kinds sharing the table are eligible. gc is the only thing that
// reclaims rows here: a thread is otherwise removed only by merging, and
// a task by nothing at all, so tasks accumulated for the life of the
// database until this stopped excluding them. The terminal-status and
// cutoff rules are the same for both -- a task's completed/cancelled/
// interrupted are terminal statuses like any other.
//
// gc never deletes a worktree; it only reports one it is about to strand.
// A selected row is reported when it is kind=thread (a task never owns a
// worktree), its worktree_path is set, and the path still exists on disk
// -- an already-cleaned-up worktree is not an orphan and is silently
// skipped, along with any os.Stat error, since that is not a failure of
// gc itself.
func selectThreads(ctx context.Context, q *sennitdb.Queries, cutoff int64, projectPath string) ([]string, []string, error) {
	rows, err := q.ListThreadsForGC(ctx)
	if err != nil {
		return nil, nil, err
	}

	var ids, orphaned []string
	for _, r := range rows {
		if projectPath != "" && r.ProjectPath != projectPath {
			continue
		}
		// Unknown statuses are deliberately retained for forward
		// compatibility rather than assumed terminal.
		if !persistedThreadStatus(r.Status).terminal() {
			continue
		}
		if r.UpdatedAt >= cutoff {
			continue
		}
		ids = append(ids, r.ID)
		if persistedThreadKind(r.Kind) == persistedKindThread && r.WorktreePath != "" {
			if _, err := os.Stat(r.WorktreePath); err == nil {
				orphaned = append(orphaned, r.WorktreePath)
			}
		}
	}
	sort.Strings(ids)
	sort.Strings(orphaned)
	return ids, orphaned, nil
}

// DeleteFunc performs the actual row deletions for a Selection, inside the
// transaction DeleteWith opened to collect it. Delete uses DeleteSelected;
// tests substitute their own to inject a failure partway through and
// assert the transaction rolls the whole selection back.
type DeleteFunc func(context.Context, *sennitdb.Queries, Selection) error

// Delete removes the selected sessions (and their messages/files/
// read_files) and threads inside a single transaction.
func Delete(ctx context.Context, conn *sql.DB, q *sennitdb.Queries, cutoff int64, projectPath string) (Selection, error) {
	return DeleteWith(ctx, conn, q, cutoff, projectPath, DeleteSelected)
}

// DeleteWith begins the immediate writer transaction before collecting the
// authoritative selection. Its lock prevents another writer from changing
// eligibility or adding descendants before the selected rows are
// committed.
func DeleteWith(ctx context.Context, conn *sql.DB, q *sennitdb.Queries, cutoff int64, projectPath string, deleteFunc DeleteFunc) (Selection, error) {
	var selection Selection
	err := sennitdb.InTx(ctx, conn, func(qtx *sennitdb.Queries) error {
		var err error
		selection, err = Collect(ctx, qtx, cutoff, projectPath)
		if err != nil {
			return fmt.Errorf("collecting history: %w", err)
		}
		return deleteFunc(ctx, qtx, selection)
	})
	if err != nil {
		return Selection{}, err
	}
	return selection, nil
}

// DeleteSelected removes every row a Selection named: explicit per-table
// deletes mirror sessionstore.Service.Delete's pattern rather than leaning
// solely on the schema's ON DELETE CASCADE, so gc keeps working even if a
// future migration ever loosens those foreign keys.
func DeleteSelected(ctx context.Context, q *sennitdb.Queries, selection Selection) error {
	for _, id := range selection.SessionIDs {
		if err := q.DeleteSessionMessages(ctx, id); err != nil {
			return fmt.Errorf("deleting messages for session %s: %w", id, err)
		}
		if err := q.DeleteSessionFiles(ctx, id); err != nil {
			return fmt.Errorf("deleting files for session %s: %w", id, err)
		}
		if err := q.DeleteSessionReadFiles(ctx, id); err != nil {
			return fmt.Errorf("deleting read_files for session %s: %w", id, err)
		}
		if err := q.DeleteSession(ctx, id); err != nil {
			return fmt.Errorf("deleting session %s: %w", id, err)
		}
	}
	for _, id := range selection.ThreadIDs {
		if err := q.DeleteThread(ctx, id); err != nil {
			return fmt.Errorf("deleting thread %s: %w", id, err)
		}
	}
	return nil
}

// Vacuum reclaims space freed by Delete. VACUUM cannot run inside a
// transaction, so it is issued as its own statement after Delete's
// transaction has committed (database/sql returns the underlying
// connection to the pool on Commit, so this is not nested inside it). The
// WAL checkpoints bracket VACUUM: the first flushes Delete's writes out of
// the WAL so VACUUM can see and compact the freed space, the second
// flushes VACUUM's own writes so the on-disk file (not just the WAL)
// reflects the smaller size afterward.
func Vacuum(ctx context.Context, conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		return fmt.Errorf("checkpointing before vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "VACUUM;"); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		return fmt.Errorf("checkpointing after vacuum: %w", err)
	}
	return nil
}
