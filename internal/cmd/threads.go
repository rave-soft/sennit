package cmd

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/spf13/cobra"
)

// threadsCmd is the CLI surface for monitoring and managing threads: the
// parallel agent work streams `ctrl+e` drives from the TUI. It exists so a
// thread running unattended (e.g. kicked off from a script, or left
// running while the TUI is closed) can still be inspected and acted on.
// Bare "sennit threads" lists, mirroring how "sennit session" defaults to no
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

// acquireWorkspace is an indirection over setupWorkspaceWithProgressBar so
// tests can substitute a fake workspace without spinning up a full
// in-process app.App. Production code never reassigns it.
var acquireWorkspace = setupWorkspaceWithProgressBar

// requireThreads runs the setup prologue shared by every threads
// subcommand: initialize the workspace (with the progress bar), and bail
// out with a uniform error if it doesn't support threads. errPrefix names
// the caller so the error reads e.g. "threads: list: this workspace...".
func requireThreads(cmd *cobra.Command, errMsg string) (context.Context, workspace.Workspace, func(), error) {
	ws, cleanup, err := acquireWorkspace(cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	if !ws.SupportsThreads() {
		cleanup()
		return nil, nil, nil, fmt.Errorf("threads: %s", errMsg)
	}

	return cmd.Context(), ws, cleanup, nil
}

// runThreadsList prints every known thread as a table (or JSON with
// --json). Goes through setupWorkspaceWithProgressBar for the same
// initialization and progress-bar behavior as the other commands.
func runThreadsList(cmd *cobra.Command, _ []string) error {
	ctx, ws, cleanup, err := requireThreads(cmd, "this workspace doesn't support threads (not a git repository, or already inside a thread's own workspace)")
	if err != nil {
		return err
	}
	defer cleanup()

	threads, err := ws.ListThreads(ctx)
	if err != nil {
		return fmt.Errorf("threads: list: %w", err)
	}

	if threadsListJSON {
		return emitJSON(cmd.OutOrStdout(), threads)
	}

	return renderThreadsTable(cmd.OutOrStdout(), threads)
}

// renderThreadsTable prints threads as a tab-aligned table, or "No
// threads." for the empty case. Split out from runThreadsList so it's
// testable without going through requireThreads/acquireWorkspace.
func renderThreadsTable(out io.Writer, threads []proto.Thread) error {
	if len(threads) == 0 {
		fmt.Fprintln(out, "No threads.")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
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
	ctx, ws, cleanup, err := requireThreads(cmd, "this workspace doesn't support threads")
	if err != nil {
		return err
	}
	defer cleanup()

	t, err := ws.CreateThread(ctx, proto.CreateThreadRequest{Name: args[0], Goal: threadsCreateGoal})
	if err != nil {
		return fmt.Errorf("threads: create: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created thread %q (branch %s)\n", t.Name, t.Branch)
	return nil
}

func runThreadsMerge(cmd *cobra.Command, args []string) error {
	ctx, ws, cleanup, err := requireThreads(cmd, "this workspace doesn't support threads")
	if err != nil {
		return err
	}
	defer cleanup()

	t, err := ws.MergeThread(ctx, args[0])
	if err != nil {
		return fmt.Errorf("threads: merge: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Thread %q: %s\n", t.Name, t.Status)
	return nil
}

func runThreadsRemove(cmd *cobra.Command, args []string) error {
	ctx, ws, cleanup, err := requireThreads(cmd, "this workspace doesn't support threads")
	if err != nil {
		return err
	}
	defer cleanup()

	if err := ws.RemoveThread(ctx, args[0], proto.RemoveThreadOptions{
		Force:        threadsRemoveForce,
		DeleteBranch: threadsRemoveDeleteBranch,
	}); err != nil {
		return fmt.Errorf("threads: remove: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed thread %q\n", args[0])
	return nil
}
