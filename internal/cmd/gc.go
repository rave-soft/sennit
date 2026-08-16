package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	sennitdb "github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/spf13/cobra"
)

// defaultHistoryRetentionDays is used when options.history_retention_days
// is unset in config. It mirrors the jsonschema default on
// config.Options.HistoryRetentionDays.
const defaultHistoryRetentionDays = 90

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Purge old session/thread history and reclaim database space",
	Long: `Delete sessions (and their messages, files, and read-file records)
whose last activity is older than the retention window, delete finished
threads (completed, merged, conflict, merge_blocked, failed, or
interrupted — never pending/running/merging) older than the same window,
then VACUUM the database and checkpoint its WAL file.

The retention window defaults to options.history_retention_days (90 days
if unset); 0 means "keep forever" and turns "braid gc" into a no-op. A
session's age is judged by its updated_at, exactly like "braid stat"'s
--since window; a session updated exactly at the cutoff is kept, since the
comparison is strict (updated_at < cutoff).

Deleting a session also deletes any agent-tool/title sub-sessions parented
to it, regardless of the child's own age. Sub-sessions old enough on their
own are deleted independently of their parent.

By default "braid gc" operates on the entire shared database, across every
project — the database is shared, so that is almost always what you want
when reclaiming disk space. Pass --project to scope the run to sessions and
threads under the current working directory only.`,
	Example: `
# See what a gc run would delete, without deleting anything
braid gc --dry-run

# Purge history older than 30 days instead of the configured default
braid gc --days 30

# Only touch the current project's history
braid gc --project
  `,
	RunE: runGC,
}

func init() {
	gcCmd.Flags().Int("days", 0, "Override options.history_retention_days (0 keeps history forever)")
	gcCmd.Flags().Bool("dry-run", false, "Report what would be deleted without deleting anything")
	gcCmd.Flags().Bool("project", false, "Scope to the current project instead of the entire shared database")
	gcCmd.Flags().Bool("json", false, "Output machine-readable JSON instead of text")
}

// gcReport summarizes one `braid gc` run, for both the human-readable and
// --json output paths.
type gcReport struct {
	DryRun            bool   `json:"dry_run"`
	RetentionDays     int    `json:"retention_days"`
	CutoffUnix        int64  `json:"cutoff_unix"`
	Scope             string `json:"scope"`
	SessionsDeleted   int    `json:"sessions_deleted"`
	MessagesDeleted   int64  `json:"messages_deleted"`
	FilesDeleted      int64  `json:"files_deleted"`
	ReadFilesDeleted  int64  `json:"read_files_deleted"`
	ThreadsDeleted    int    `json:"threads_deleted"`
	DBSizeBeforeBytes int64  `json:"db_size_before_bytes"`
	DBSizeAfterBytes  int64  `json:"db_size_after_bytes,omitempty"`
}

func runGC(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	projectScope, _ := cmd.Flags().GetBool("project")
	jsonOut, _ := cmd.Flags().GetBool("json")

	dataDir, _ := cmd.Flags().GetString("data-dir")
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}
	cfg, err := config.Init(cwd, dataDir, false)
	if err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}

	retentionDays := defaultHistoryRetentionDays
	if v := cfg.Config().Options.HistoryRetentionDays; v != nil {
		retentionDays = *v
	}
	if cmd.Flags().Changed("days") {
		retentionDays, _ = cmd.Flags().GetInt("days")
	}

	report := gcReport{
		DryRun:        dryRun,
		RetentionDays: retentionDays,
		Scope:         "all-projects",
	}
	var projectPath string
	if projectScope {
		projectPath = cwd
		report.Scope = projectPath
	}

	out := cmd.OutOrStdout()

	// retentionDays <= 0 means "keep forever" -- braid gc has nothing to
	// do and, since nothing changes, nothing to VACUUM either.
	if retentionDays <= 0 {
		if jsonOut {
			return json.NewEncoder(out).Encode(report)
		}
		fmt.Fprintln(out, "History retention is disabled (history_retention_days = 0); nothing to do.")
		return nil
	}

	dbPath := filepath.Join(config.GlobalDBDir(), brand.DBFile)
	if info, err := os.Stat(dbPath); err == nil {
		report.DBSizeBeforeBytes = info.Size()
	}

	conn, err := sennitdb.Connect(ctx, config.GlobalDBDir())
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer sennitdb.Release(config.GlobalDBDir()) //nolint:errcheck // best-effort refcount release on exit
	queries := sennitdb.New(conn)

	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	report.CutoffUnix = cutoff

	if dryRun {
		selection, err := gcCollect(ctx, queries, conn, cutoff, projectPath)
		if err != nil {
			return fmt.Errorf("failed to select history for gc: %w", err)
		}
		selection.apply(&report)
		if jsonOut {
			return json.NewEncoder(out).Encode(report)
		}
		renderGCReport(out, report)
		return nil
	}

	selection, err := gcDelete(ctx, conn, queries, cutoff, projectPath)
	if err != nil {
		return fmt.Errorf("failed to delete gc'd history: %w", err)
	}
	selection.apply(&report)
	if len(selection.sessionIDs) > 0 || len(selection.threadIDs) > 0 {
		if err := gcVacuum(ctx, conn); err != nil {
			return fmt.Errorf("failed to reclaim database space: %w", err)
		}
		if info, err := os.Stat(dbPath); err == nil {
			report.DBSizeAfterBytes = info.Size()
		}
	} else {
		report.DBSizeAfterBytes = report.DBSizeBeforeBytes
	}

	if jsonOut {
		return json.NewEncoder(out).Encode(report)
	}
	renderGCReport(out, report)
	return nil
}

type gcSelection struct {
	sessionIDs       []string
	threadIDs        []string
	messagesDeleted  int64
	filesDeleted     int64
	readFilesDeleted int64
}

func (s gcSelection) apply(report *gcReport) {
	report.SessionsDeleted = len(s.sessionIDs)
	report.ThreadsDeleted = len(s.threadIDs)
	report.MessagesDeleted = s.messagesDeleted
	report.FilesDeleted = s.filesDeleted
	report.ReadFilesDeleted = s.readFilesDeleted
}

// gcRowQuerier is the slice of database/sql shared by *sql.DB and *sql.Tx
// that gcCountDependents needs; it lets the same counting code run against
// the plain connection (dry run) and inside the writer transaction.
type gcRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// gcCollect selects history and counts its dependent rows using q's (and
// db's) current snapshot. Authoritative callers pass transaction-bound
// queries plus the transaction; dry runs deliberately pass ordinary
// database handles.
func gcCollect(ctx context.Context, q *sennitdb.Queries, db gcRowQuerier, cutoff int64, projectPath string) (gcSelection, error) {
	sessionIDs, err := gcSelectSessions(ctx, q, cutoff, projectPath)
	if err != nil {
		return gcSelection{}, err
	}
	threadIDs, err := gcSelectThreads(ctx, q, cutoff, projectPath)
	if err != nil {
		return gcSelection{}, err
	}

	selection := gcSelection{sessionIDs: sessionIDs, threadIDs: threadIDs}
	selection.messagesDeleted, selection.filesDeleted, selection.readFilesDeleted, err = gcCountDependents(ctx, db, sessionIDs)
	if err != nil {
		return gcSelection{}, err
	}
	return selection, nil
}

// gcCountDependentsChunk bounds the number of bound parameters per
// aggregate COUNT query, comfortably under SQLite's variable limit. Var so
// the chunking path is testable without thousands of fixture rows.
var gcCountDependentsChunk = 500

// gcCountDependents totals the messages/files/read_files rows belonging to
// sessionIDs with one aggregate COUNT per table per chunk — not three
// queries per session, which inside the writer transaction would hold the
// database's write lock across 3N reads before the first delete.
func gcCountDependents(ctx context.Context, db gcRowQuerier, sessionIDs []string) (messages, files, readFiles int64, err error) {
	for start := 0; start < len(sessionIDs); start += gcCountDependentsChunk {
		chunk := sessionIDs[start:min(start+gcCountDependentsChunk, len(sessionIDs))]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		for _, target := range []struct {
			table string
			total *int64
		}{
			{"messages", &messages},
			{"files", &files},
			{"read_files", &readFiles},
		} {
			var n int64
			query := "SELECT COUNT(*) FROM " + target.table + " WHERE session_id IN (" + placeholders + ")"
			if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
				return 0, 0, 0, fmt.Errorf("counting %s rows for gc: %w", target.table, err)
			}
			*target.total += n
		}
	}
	return messages, files, readFiles, nil
}

// gcSelectSessions returns the IDs of every session `braid gc` should
// delete: every session whose updated_at is strictly older than cutoff
// (scoped to projectPath when non-empty), expanded to include any
// agent-tool/title sub-session parented to a selected session, regardless
// of the sub-session's own age. The expansion runs to a fixed point so a
// sub-session's own sub-sessions are swept up too.
func gcSelectSessions(ctx context.Context, q *sennitdb.Queries, cutoff int64, projectPath string) ([]string, error) {
	rows, err := q.ListSessionsForGC(ctx)
	if err != nil {
		return nil, err
	}

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
		if !inScope[r.ID] {
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

// gcSelectThreads returns the IDs of finished threads (never
// pending/running/merging) whose updated_at is strictly older than
// cutoff, scoped to projectPath when non-empty.
func gcSelectThreads(ctx context.Context, q *sennitdb.Queries, cutoff int64, projectPath string) ([]string, error) {
	rows, err := q.ListThreadsForGC(ctx)
	if err != nil {
		return nil, err
	}

	var ids []string
	for _, r := range rows {
		if projectPath != "" && r.ProjectPath != projectPath {
			continue
		}
		// Unknown statuses are neither active nor terminal and are
		// deliberately retained for forward compatibility (see
		// thread.Status.Terminal).
		if !thread.Status(r.Status).Terminal() {
			continue
		}
		if r.UpdatedAt < cutoff {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// gcDelete removes the selected sessions (and their messages/files/
// read_files) and threads inside a single transaction. Explicit per-table
// deletes mirror session.Service.Delete's pattern rather than leaning
// solely on the schema's ON DELETE CASCADE, so gc keeps working even if a
// future migration ever loosens those foreign keys.
func gcDelete(ctx context.Context, conn *sql.DB, q *sennitdb.Queries, cutoff int64, projectPath string) (gcSelection, error) {
	return gcDeleteWith(ctx, conn, q, cutoff, projectPath, gcDeleteSelected)
}

type gcDeleteFunc func(context.Context, *sennitdb.Queries, gcSelection) error

// gcDeleteWith begins the immediate writer transaction before collecting the
// authoritative selection. Its lock prevents another writer from changing
// eligibility or adding descendants before the selected rows are committed.
func gcDeleteWith(ctx context.Context, conn *sql.DB, q *sennitdb.Queries, cutoff int64, projectPath string, deleteFunc gcDeleteFunc) (gcSelection, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return gcSelection{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := q.WithTx(tx)
	selection, err := gcCollect(ctx, qtx, tx, cutoff, projectPath)
	if err != nil {
		return gcSelection{}, fmt.Errorf("collecting history: %w", err)
	}
	if err := deleteFunc(ctx, qtx, selection); err != nil {
		return gcSelection{}, err
	}
	if err := tx.Commit(); err != nil {
		return gcSelection{}, err
	}
	return selection, nil
}

func gcDeleteSelected(ctx context.Context, q *sennitdb.Queries, selection gcSelection) error {
	for _, id := range selection.sessionIDs {
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
	for _, id := range selection.threadIDs {
		if err := q.DeleteThread(ctx, id); err != nil {
			return fmt.Errorf("deleting thread %s: %w", id, err)
		}
	}

	return nil
}

// gcVacuum reclaims space freed by gcDelete. VACUUM cannot run inside a
// transaction, so it is issued as its own statement after gcDelete's
// transaction has committed (database/sql returns the underlying
// connection to the pool on Commit, so this is not nested inside it). The
// WAL checkpoints bracket VACUUM: the first flushes gcDelete's writes out
// of the WAL so VACUUM can see and compact the freed space, the second
// flushes VACUUM's own writes so the on-disk file (not just the WAL)
// reflects the smaller size that os.Stat reports afterward.
func gcVacuum(ctx context.Context, conn *sql.DB) error {
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

func renderGCReport(w io.Writer, r gcReport) {
	verb := "Deleted"
	if r.DryRun {
		verb = "Would delete"
	}
	fmt.Fprintf(w, "Scope: %s\n", r.Scope)
	fmt.Fprintf(w, "Retention: %d days (cutoff %s)\n", r.RetentionDays, time.Unix(r.CutoffUnix, 0).Format(time.RFC3339))
	fmt.Fprintf(w, "%s %s sessions, %s messages, %s files, %s read_files, %s threads\n",
		verb,
		humanize.Comma(int64(r.SessionsDeleted)),
		humanize.Comma(r.MessagesDeleted),
		humanize.Comma(r.FilesDeleted),
		humanize.Comma(r.ReadFilesDeleted),
		humanize.Comma(int64(r.ThreadsDeleted)))
	fmt.Fprintf(w, "Database size: %s", humanize.Bytes(uint64(r.DBSizeBeforeBytes)))
	if !r.DryRun {
		fmt.Fprintf(w, " -> %s", humanize.Bytes(uint64(r.DBSizeAfterBytes)))
	}
	fmt.Fprintln(w)
}
