package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"text/tabwriter"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/spf13/cobra"
)

// attachLocalThreads gives a's single-process CLI workspace ownership of a
// thread manager, when it's warranted: cwd must be a git repository's
// toplevel. A workspace opened in a subdirectory of a repo is not given
// one, since a thread's worktrees and branches are rooted at the repo
// toplevel, not at an arbitrary subdirectory a session happens to be
// running from.
//
// This is best-effort: failures are logged, not returned, so a broken
// thread store (or a non-git cwd) never blocks the workspace it would
// have belonged to from starting.
func attachLocalThreads(ctx context.Context, a *app.App, cwd string) {
	top, err := git.TopLevel(ctx, cwd)
	if err != nil || top != cwd {
		return
	}

	dbDir := config.GlobalDBDir()
	conn, err := db.Connect(ctx, dbDir)
	if err != nil {
		slog.Warn("Failed to open thread store, threads unavailable", "error", err)
		return
	}
	worktreeDir := ""
	if opts := a.Config().Options.Threads; opts != nil {
		worktreeDir = opts.WorktreeDir
	}
	mgr := thread.NewManager(thread.ManagerOptions{
		Store: thread.NewStore(db.New(conn), a.Store().WorkingDir()),
		Spawner: thread.NewLocalSpawner(
			func() map[string]config.Agent { return a.Config().UserAgents() },
			func() bool { return a.Store().Overrides().SkipPermissionRequests },
		),
		RepoRoot:    top,
		WorktreeDir: worktreeDir,
		Context:     ctx,
	})
	if err := a.AddShutdownHook(func(context.Context) error {
		return mgr.Shutdown(context.Background())
	}); err != nil {
		slog.Warn("Failed to register thread manager shutdown", "error", err)
		_ = db.Release(dbDir)
		return
	}
	if err := a.AddCriticalCleanup(func(context.Context) error {
		return db.Release(dbDir)
	}); err != nil {
		slog.Warn("Failed to register thread database cleanup", "error", err)
		_ = mgr.Shutdown(context.Background())
		_ = db.Release(dbDir)
		return
	}
	if err := mgr.Recover(ctx); err != nil {
		slog.Warn("Failed to recover thread state", "error", err)
	}

	// SetThreads makes the thread_* agent tools available to the main
	// coordinator; SetThreadManager gives internal/workspace and
	// internal/server (later steps) access to the full *thread.Manager;
	// ForwardEvents fans thread lifecycle events into this workspace's
	// event stream (app.Subscribe locally, app.Events/SSE remotely).
	a.SetThreads(thread.AsAgentToolManager(mgr))
	a.SetThreadManager(mgr)
	app.ForwardEvents(a, "thread", mgr.Subscribe)
}

// threadsCmd is the CLI surface for monitoring and managing threads: the
// parallel agent work streams `ctrl+e` drives from the TUI. It exists so a
// thread running unattended (e.g. kicked off from a script, or left
// running while the TUI is closed) can still be inspected and acted on.
// Bare "braid threads" lists, mirroring how "braid session" defaults to no
// subcommand meaning nothing (session requires "list") — threads instead
// defaults straight to listing since that's the overwhelmingly common
// case: a user checking "what's still running".
var threadsCmd = &cobra.Command{
	Use:     "threads",
	Aliases: []string{"thread", "t"},
	Short:   "Monitor and manage work threads",
	Long: `Monitor and manage threads: parallel agent work streams, each
running in its own git worktree and branch (see "ctrl+e" in the TUI).

With no subcommand, lists every known thread — this is the common case,
so it needs no subcommand of its own.`,
	RunE: runThreadsList,
}

var threadsListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all threads",
	RunE:    runThreadsList,
}

var threadsCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new thread",
	Long:  "Create a new thread: a parallel agent work stream in its own git worktree and branch.",
	Args:  cobra.ExactArgs(1),
	RunE:  runThreadsCreate,
}

var threadsMergeCmd = &cobra.Command{
	Use:   "merge <name>",
	Short: "Merge a thread's branch back into its base branch",
	Args:  cobra.ExactArgs(1),
	RunE:  runThreadsMerge,
}

var threadsRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a thread",
	Args:    cobra.ExactArgs(1),
	RunE:    runThreadsRemove,
}

var (
	threadsListJSON           bool
	threadsCreateGoal         string
	threadsRemoveForce        bool
	threadsRemoveDeleteBranch bool
)

func init() {
	threadsCmd.Flags().BoolVar(&threadsListJSON, "json", false, "output in JSON format")
	threadsListCmd.Flags().BoolVar(&threadsListJSON, "json", false, "output in JSON format")
	threadsCreateCmd.Flags().StringVar(&threadsCreateGoal, "goal", "", "goal prompt to dispatch immediately")
	threadsRemoveCmd.Flags().BoolVar(&threadsRemoveForce, "force", false, "remove even if running/merging or dirty")
	threadsRemoveCmd.Flags().BoolVar(&threadsRemoveDeleteBranch, "delete-branch", false, "also delete the thread's git branch")

	threadsCmd.AddCommand(threadsListCmd, threadsCreateCmd, threadsMergeCmd, threadsRemoveCmd)
}

// runThreadsList prints every known thread as a table (or JSON with
// --json). Goes through setupWorkspace so it transparently works whether
// braid is running in local or client/server mode.
func runThreadsList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := workspace.InitializeThreadsCapability(ctx, ws); err != nil {
		return fmt.Errorf("threads: determine capability: %w", err)
	}
	if !ws.SupportsThreads() {
		return fmt.Errorf("threads: this workspace doesn't support threads (not a git repository, or already inside a thread's own workspace)")
	}

	threads, err := ws.ListThreads(ctx)
	if err != nil {
		return fmt.Errorf("threads: list: %w", err)
	}

	if threadsListJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetEscapeHTML(false)
		return enc.Encode(threads)
	}

	if len(threads) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No threads.")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tBRANCH\tBASE\tUPDATED\tGOAL")
	for _, t := range threads {
		goal := t.Goal
		if len(goal) > 60 {
			goal = goal[:59] + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, t.Status, t.Branch, t.BaseBranch,
			humanize.Time(time.Unix(t.UpdatedAt, 0)), goal)
	}
	return w.Flush()
}

func runThreadsCreate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := workspace.InitializeThreadsCapability(ctx, ws); err != nil {
		return fmt.Errorf("threads: determine capability: %w", err)
	}
	if !ws.SupportsThreads() {
		return fmt.Errorf("threads: this workspace doesn't support threads")
	}

	t, err := ws.CreateThread(ctx, proto.CreateThreadRequest{Name: args[0], Goal: threadsCreateGoal})
	if err != nil {
		return fmt.Errorf("threads: create: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created thread %q (branch %s)\n", t.Name, t.Branch)
	return nil
}

func runThreadsMerge(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := workspace.InitializeThreadsCapability(ctx, ws); err != nil {
		return fmt.Errorf("threads: determine capability: %w", err)
	}
	if !ws.SupportsThreads() {
		return fmt.Errorf("threads: this workspace doesn't support threads")
	}

	t, err := ws.MergeThread(ctx, args[0])
	if err != nil {
		return fmt.Errorf("threads: merge: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Thread %q: %s\n", t.Name, t.Status)
	return nil
}

func runThreadsRemove(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := workspace.InitializeThreadsCapability(ctx, ws); err != nil {
		return fmt.Errorf("threads: determine capability: %w", err)
	}
	if !ws.SupportsThreads() {
		return fmt.Errorf("threads: this workspace doesn't support threads")
	}

	if err := ws.RemoveThread(ctx, args[0], proto.RemoveThreadOptions{
		Force:        threadsRemoveForce,
		DeleteBranch: threadsRemoveDeleteBranch,
	}); err != nil {
		return fmt.Errorf("threads: remove: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed thread %q\n", args[0])
	return nil
}
