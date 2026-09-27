package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/spf13/cobra"
)

// psCmd reports what this project's sennit daemon is doing right now:
// busy sessions, pending permission/question prompts, and any live
// threads or tasks (CLIENT-SERVER.md, PR 2.3). It never starts a daemon
// -- a project with none running is simply reported idle-by-absence,
// exit 0, the way `ps` for an absent process is not itself an error.
var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "Show what this project's sennit daemon is doing",
	RunE: func(cmd *cobra.Command, args []string) error {
		debug, _ := cmd.Flags().GetBool("debug")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		jsonOut, _ := cmd.Flags().GetBool("json")
		ctx := cmd.Context()

		var client psSource
		if target, ok, err := remoteTargetFlag(cmd); err != nil {
			return err
		} else if ok {
			c, _, cleanup, err := connectRemoteWorkspace(ctx, target, remoteDialerOptions(cmd), dataDir, debug)
			if err != nil {
				return err
			}
			defer cleanup()
			client = c
		} else {
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			socketPath, running, err := supervisor.ProbeRunning(ctx, cwd, supervisor.Options{DataDir: dataDir, Debug: debug})
			if err != nil {
				return err
			}
			if !running {
				fmt.Fprintf(cmd.OutOrStdout(), "no daemon running for %s\n", cwd)
				return nil
			}

			c, _, cleanup, err := connectDaemonWorkspace(ctx, cwd, dataDir, debug, socketPath)
			if err != nil {
				return err
			}
			defer cleanup()
			client = c
		}

		report, err := collectPS(ctx, client)
		if err != nil {
			return err
		}

		if jsonOut {
			return emitJSON(cmd.OutOrStdout(), report)
		}
		printPS(cmd.OutOrStdout(), report)
		return nil
	},
}

func init() {
	psCmd.Flags().Bool("json", false, "output in JSON format")
	rootCmd.AddCommand(psCmd)
}

// psSource is the narrow set of reads collectPS needs from a connected
// daemon client, kept separate from workspace.Workspace so a test can
// fake exactly this and nothing else. *grpcws.Client satisfies it
// structurally.
type psSource interface {
	AgentActivity() workspace.AgentActivity
	PendingPrompts(ctx context.Context) (workspace.PendingPrompts, error)
	ListSessions(ctx context.Context) ([]session.Session, error)
	SupportsThreads() bool
	ListThreads(ctx context.Context) ([]proto.Thread, error)
	SupportsTasks() bool
	ListTasks(ctx context.Context) ([]proto.Thread, error)
}

// psReport is `sennit ps`'s whole answer, and its --json shape.
type psReport struct {
	BusySessions []psBusySession `json:"busy_sessions"`
	Permissions  []psPermission  `json:"permissions"`
	Questions    []psQuestion    `json:"questions"`
	Threads      []psDelegation  `json:"threads,omitempty"`
	Tasks        []psDelegation  `json:"tasks,omitempty"`
}

type psBusySession struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	QueuedPrompts int    `json:"queued_prompts"`
}

type psPermission struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
}

type psQuestion struct {
	ID            string `json:"id"`
	SessionID     string `json:"session_id"`
	QuestionCount int    `json:"question_count"`
}

type psDelegation struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// collectPS gathers a psReport from src. Every field is a read that can
// legitimately be empty (no busy sessions, no pending prompts, threads/
// tasks unsupported) -- only ListSessions/ListThreads/ListTasks erroring
// is reported up, since those are the reads that can genuinely fail
// rather than just come back empty.
func collectPS(ctx context.Context, src psSource) (psReport, error) {
	var report psReport

	activity := src.AgentActivity()
	if len(activity.BusySessions) > 0 {
		sessions, err := src.ListSessions(ctx)
		if err != nil {
			return psReport{}, fmt.Errorf("listing sessions: %w", err)
		}
		titles := make(map[string]string, len(sessions))
		for _, s := range sessions {
			titles[s.ID] = s.Title
		}
		for _, id := range activity.BusySessions {
			report.BusySessions = append(report.BusySessions, psBusySession{
				ID:            id,
				Title:         titles[id],
				QueuedPrompts: len(activity.QueuedPrompts[id]),
			})
		}
	}

	prompts, err := src.PendingPrompts(ctx)
	if err != nil {
		return psReport{}, fmt.Errorf("reading pending prompts: %w", err)
	}
	for _, p := range prompts.Permissions {
		report.Permissions = append(report.Permissions, psPermission{
			ID:          p.ID,
			SessionID:   p.SessionID,
			ToolName:    p.ToolName,
			Description: p.Description,
		})
	}
	for _, q := range prompts.Questions {
		report.Questions = append(report.Questions, psQuestion{
			ID:            q.ID,
			SessionID:     q.SessionID,
			QuestionCount: len(q.Questions),
		})
	}

	if src.SupportsThreads() {
		threads, err := src.ListThreads(ctx)
		if err != nil {
			return psReport{}, fmt.Errorf("listing threads: %w", err)
		}
		for _, th := range threads {
			report.Threads = append(report.Threads, psDelegation{ID: th.ID, Title: th.Name, Status: th.Status})
		}
	}

	if src.SupportsTasks() {
		tasks, err := src.ListTasks(ctx)
		if err != nil {
			return psReport{}, fmt.Errorf("listing tasks: %w", err)
		}
		for _, tk := range tasks {
			report.Tasks = append(report.Tasks, psDelegation{ID: tk.ID, Title: tk.Name, Status: tk.Status})
		}
	}

	return report, nil
}

// printPS renders a psReport as plain, flat text sections -- one per
// non-empty category, in the same "nothing to say when there's nothing
// there" spirit as `sennit gc`'s summary output.
func printPS(w io.Writer, report psReport) {
	if len(report.BusySessions) == 0 && len(report.Permissions) == 0 &&
		len(report.Questions) == 0 && len(report.Threads) == 0 && len(report.Tasks) == 0 {
		fmt.Fprintln(w, "Daemon is idle: no busy sessions, pending prompts, threads or tasks.")
		return
	}

	if len(report.BusySessions) > 0 {
		fmt.Fprintln(w, "Busy sessions:")
		for _, s := range report.BusySessions {
			hash := session.HashID(s.ID)[:7] // ok: ascii — a hex session hash
			fmt.Fprintf(w, "  %s  %s  (queued: %d)\n", hash, s.Title, s.QueuedPrompts)
		}
	}
	if len(report.Permissions) > 0 {
		fmt.Fprintln(w, "Pending permission requests:")
		for _, p := range report.Permissions {
			hash := session.HashID(p.SessionID)[:7] // ok: ascii — a hex session hash
			fmt.Fprintf(w, "  %s  session=%s  %s\n", p.ToolName, hash, p.Description)
		}
	}
	if len(report.Questions) > 0 {
		fmt.Fprintln(w, "Pending questions:")
		for _, q := range report.Questions {
			hash := session.HashID(q.SessionID)[:7] // ok: ascii — a hex session hash
			fmt.Fprintf(w, "  session=%s  %d question(s)\n", hash, q.QuestionCount)
		}
	}
	if len(report.Threads) > 0 {
		fmt.Fprintln(w, "Threads:")
		for _, t := range report.Threads {
			fmt.Fprintf(w, "  %s  %s  %s\n", t.ID, t.Status, t.Title)
		}
	}
	if len(report.Tasks) > 0 {
		fmt.Fprintln(w, "Tasks:")
		for _, t := range report.Tasks {
			fmt.Fprintf(w, "  %s  %s  %s\n", t.ID, t.Status, t.Title)
		}
	}
}
