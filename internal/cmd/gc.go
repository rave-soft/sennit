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
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rave-soft/braid/internal/config"
	braiddb "github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/thread"
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

	dbPath := filepath.Join(config.GlobalDBDir(), "braid.db")
	if info, err := os.Stat(dbPath); err == nil {
		report.DBSizeBeforeBytes = info.Size()
	}

	conn, err := braiddb.Connect(ctx, config.GlobalDBDir())
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer braiddb.Release(config.GlobalDBDir()) //nolint:errcheck // best-effort refcount release on exit
	queries := braiddb.New(conn)

	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	report.CutoffUnix = cutoff

	sessionIDs, err := gcSelectSessions(ctx, queries, cutoff, projectPath)
	if err != nil {
		return fmt.Errorf("failed to select sessions for gc: %w", err)
	}
	threadIDs, err := gcSelectThreads(ctx, queries, cutoff, projectPath)
	if err != nil {
		return fmt.Errorf("failed to select threads for gc: %w", err)
	}

	report.SessionsDeleted = len(sessionIDs)
	report.ThreadsDeleted = len(threadIDs)
	for _, id := range sessionIDs {
		n, err := queries.CountSessionMessages(ctx, id)
		if err != nil {
			return fmt.Errorf("failed to count messages for session %s: %w", id, err)
		}
		report.MessagesDeleted += n
		n, err = queries.CountSessionFiles(ctx, id)
		if err != nil {
			return fmt.Errorf("failed to count files for session %s: %w", id, err)
		}
		report.FilesDeleted += n
		n, err = queries.CountSessionReadFiles(ctx, id)
		if err != nil {
			return fmt.Errorf("failed to count read_files for session %s: %w", id, err)
		}
		report.ReadFilesDeleted += n
	}

	if dryRun {
		if jsonOut {
			return json.NewEncoder(out).Encode(report)
		}
		renderGCReport(out, report)
		return nil
	}

	if len(sessionIDs) > 0 || len(threadIDs) > 0 {
		if err := gcDelete(ctx, conn, queries, sessionIDs, threadIDs); err != nil {
			return fmt.Errorf("failed to delete gc'd history: %w", err)
		}
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

// gcSelectSessions returns the IDs of every session `braid gc` should
// delete: every session whose updated_at is strictly older than cutoff
// (scoped to projectPath when non-empty), expanded to include any
// agent-tool/title sub-session parented to a selected session, regardless
// of the sub-session's own age. The expansion runs to a fixed point so a
// sub-session's own sub-sessions are swept up too.
func gcSelectSessions(ctx context.Context, q *braiddb.Queries, cutoff int64, projectPath string) ([]string, error) {
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
func gcSelectThreads(ctx context.Context, q *braiddb.Queries, cutoff int64, projectPath string) ([]string, error) {
	rows, err := q.ListThreadsForGC(ctx)
	if err != nil {
		return nil, err
	}

	var ids []string
	for _, r := range rows {
		if projectPath != "" && r.ProjectPath != projectPath {
			continue
		}
		if gcThreadIsActive(thread.Status(r.Status)) {
			continue
		}
		if r.UpdatedAt < cutoff {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// gcThreadIsActive reports whether a thread is still in flight and must
// never be deleted by gc, regardless of age.
func gcThreadIsActive(status thread.Status) bool {
	switch status {
	case thread.StatusPending, thread.StatusRunning, thread.StatusMerging:
		return true
	default:
		return false
	}
}

// gcDelete removes the selected sessions (and their messages/files/
// read_files) and threads inside a single transaction. Explicit per-table
// deletes mirror session.Service.Delete's pattern rather than leaning
// solely on the schema's ON DELETE CASCADE, so gc keeps working even if a
// future migration ever loosens those foreign keys.
func gcDelete(ctx context.Context, conn *sql.DB, q *braiddb.Queries, sessionIDs, threadIDs []string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := q.WithTx(tx)
	for _, id := range sessionIDs {
		if err := qtx.DeleteSessionMessages(ctx, id); err != nil {
			return fmt.Errorf("deleting messages for session %s: %w", id, err)
		}
		if err := qtx.DeleteSessionFiles(ctx, id); err != nil {
			return fmt.Errorf("deleting files for session %s: %w", id, err)
		}
		if err := qtx.DeleteSessionReadFiles(ctx, id); err != nil {
			return fmt.Errorf("deleting read_files for session %s: %w", id, err)
		}
		if err := qtx.DeleteSession(ctx, id); err != nil {
			return fmt.Errorf("deleting session %s: %w", id, err)
		}
	}
	for _, id := range threadIDs {
		if err := qtx.DeleteThread(ctx, id); err != nil {
			return fmt.Errorf("deleting thread %s: %w", id, err)
		}
	}

	return tx.Commit()
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
