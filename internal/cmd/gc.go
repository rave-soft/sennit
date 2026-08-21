package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	sennitgc "github.com/rave-soft/sennit/internal/gc"
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
if unset); 0 means "keep forever" and turns "sennit gc" into a no-op. A
session's age is judged by its updated_at, exactly like "sennit stat"'s
--since window; a session updated exactly at the cutoff is kept, since the
comparison is strict (updated_at < cutoff).

Deleting a session also deletes any agent-tool/title sub-sessions parented
to it, regardless of the child's own age. Sub-sessions old enough on their
own are deleted independently of their parent.

By default "sennit gc" operates on the entire shared database, across every
project — the database is shared, so that is almost always what you want
when reclaiming disk space. Pass --project to scope the run to sessions and
threads under the current working directory only.

A deleted thread's git worktree is reported but never removed; run
"git worktree remove <path>" yourself to clean up the ones "sennit gc"
leaves behind.`,
	Example: `
# See what a gc run would delete, without deleting anything
sennit gc --dry-run

# Purge history older than 30 days instead of the configured default
sennit gc --days 30

# Only touch the current project's history
sennit gc --project
  `,
	RunE: runGC,
}

func init() {
	gcCmd.Flags().Int("days", 0, "Override options.history_retention_days (0 keeps history forever)")
	gcCmd.Flags().Bool("dry-run", false, "Report what would be deleted without deleting anything")
	gcCmd.Flags().Bool("project", false, "Scope to the current project instead of the entire shared database")
	gcCmd.Flags().Bool("json", false, "Output machine-readable JSON instead of text")
}

// gcReport summarizes one `sennit gc` run, for both the human-readable and
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

	// OrphanedWorktrees lists the git worktrees a deleted thread owned that
	// gc found on disk. gc reports these but never removes them -- see
	// internal/gc.
	OrphanedWorktrees []string `json:"orphaned_worktrees,omitempty"`
}

// applyResult copies a gc.Report's counts and orphan list into r, leaving
// r's CLI-only fields (DryRun, RetentionDays, CutoffUnix, Scope, DB size)
// untouched.
func (r *gcReport) applyResult(result sennitgc.Report) {
	r.SessionsDeleted = result.SessionsDeleted
	r.MessagesDeleted = result.MessagesDeleted
	r.FilesDeleted = result.FilesDeleted
	r.ReadFilesDeleted = result.ReadFilesDeleted
	r.ThreadsDeleted = result.ThreadsDeleted
	r.OrphanedWorktrees = result.OrphanedWorktrees
}

func runGC(cmd *cobra.Command, _ []string) error {
	ctx := cmdContext(cmd)

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	projectScope, _ := cmd.Flags().GetBool("project")
	jsonOut, _ := cmd.Flags().GetBool("json")

	cwd, cfg, err := initConfig(cmd, false)
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

	// retentionDays <= 0 means "keep forever" -- sennit gc has nothing to
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

	queries, conn, cleanup, err := connectDB(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	report.CutoffUnix = cutoff

	result, err := sennitgc.Run(ctx, sennitgc.Deps{Queries: queries, Conn: conn}, sennitgc.Policy{
		Cutoff:      cutoff,
		ProjectPath: projectPath,
		DryRun:      dryRun,
	})
	if err != nil {
		return err
	}
	report.applyResult(result)

	if !dryRun {
		if result.Vacuumed {
			if info, err := os.Stat(dbPath); err == nil {
				report.DBSizeAfterBytes = info.Size()
			}
		} else {
			report.DBSizeAfterBytes = report.DBSizeBeforeBytes
		}
	}

	if jsonOut {
		return json.NewEncoder(out).Encode(report)
	}
	renderGCReport(out, report)
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

	if len(r.OrphanedWorktrees) > 0 {
		orphanVerb := "Orphaned"
		if r.DryRun {
			orphanVerb = "Would orphan"
		}
		fmt.Fprintf(w, "%s %s git worktree(s) (gc does not remove worktrees):\n",
			orphanVerb, humanize.Comma(int64(len(r.OrphanedWorktrees))))
		for _, path := range r.OrphanedWorktrees {
			fmt.Fprintf(w, "  %s\n", path)
		}
		fmt.Fprintln(w, "Run `git worktree remove <path>` to clean these up.")
	}
}
