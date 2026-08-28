package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rave-soft/sennit/internal/event"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/stats/gather"
	"github.com/spf13/cobra"
)

// statCmd prints usage breakdowns by model, agent, project, and skill
// straight to the terminal, in tokens and time, for use in scripts and
// quick checks, plus an opt-in latency section covering the two internal
// handoffs that can silently get slower without failing. It absorbed the
// older `stats` (plural) command, which
// rendered an HTML dashboard and opened it in a browser; `stats` is kept
// only as an alias for muscle memory, and `stat` never touches a browser.
var statCmd = &cobra.Command{
	Use:     "stat",
	Aliases: []string{"stats"},
	Short:   "Show usage statistics as terminal tables",
	Long: `Show usage statistics as terminal tables, broken down by model,
subagent, project, and skill, in tokens and time. Also available as
"sennit stats" for backwards compatibility.

Caveats baked into this data:

  - Token counts are only stored per session, not per message. When a
    session used a single model throughout, that session's tokens/cost are
    attributed to that model exactly. When a session used more than one
    model, its tokens/cost are split across those models proportionally to
    each model's share of the session's assistant messages; such rows are
    marked approximate (~) since this is an estimate, not an exact count.
    Message counts and time are always exact, since they come from
    per-message timestamps.

  - Subagent sessions are grouped by their session title, which is the
    best available proxy for "agent name": sessions delegated through the
    generic "task" tool all collapse into a single "New Agent Session"
    bucket, while custom agents (defined in config) get their own name.

  - The latency section is opt-in via --by latency and reports two
    internal handoffs: how long a steering message waited between being
    queued and being folded into a step, and how long a finished
    background delegation waited before its completion reached the
    parent. Both waits are dominated by how busy the parent session was,
    so a long tail on a session full of long turns is expected; what a
    regression looks like is the P50 rising.`,
	RunE: runStat,
}

func init() {
	statCmd.Flags().String("by", "", "Show only one section: models, agents, projects, skills, or latency (default: all except latency)")
	statCmd.Flags().String("since", "30d", "Time window: 7d, 30d, or all")
	statCmd.Flags().Bool("json", false, "Output machine-readable JSON instead of tables")
	statCmd.Flags().Bool("all-projects", false, "With --by projects, aggregate across every known project")
}

// statSince resolves the --since flag to a unix timestamp lower bound.
// "all" resolves to 0 (the unix epoch), which predates any real Sennit
// data and so is effectively unfiltered.
func statSince(since string) (int64, error) {
	switch since {
	case "7d":
		return time.Now().Add(-7 * 24 * time.Hour).Unix(), nil
	case "30d":
		return time.Now().Add(-30 * 24 * time.Hour).Unix(), nil
	case "all":
		return 0, nil
	default:
		return 0, fmt.Errorf("invalid --since value %q: must be one of 7d, 30d, all", since)
	}
}

// statOutput is the top-level shape for --json output. Only the sections
// actually requested via --by are populated. The field types come from
// internal/stats, so the JSON keys are that package's and stay identical
// to what this command emitted when it owned them.
type statOutput struct {
	Since    string          `json:"since"`
	Models   []stats.Model   `json:"models,omitempty"`
	Agents   []stats.Agent   `json:"agents,omitempty"`
	Projects []stats.Project `json:"projects,omitempty"`
	Skills   []stats.Skill   `json:"skills,omitempty"`
	Latency  []stats.Latency `json:"latency,omitempty"`
	Summary  *stats.Project  `json:"summary,omitempty"`
}

func runStat(cmd *cobra.Command, _ []string) error {
	ctx := cmdContext(cmd)

	by, _ := cmd.Flags().GetString("by")
	switch by {
	case "", "models", "agents", "projects", "skills", "latency":
	default:
		return fmt.Errorf("invalid --by value %q: must be one of models, agents, projects, skills, latency", by)
	}

	sinceFlag, _ := cmd.Flags().GetString("since")
	since, err := statSince(sinceFlag)
	if err != nil {
		return err
	}

	jsonOut, _ := cmd.Flags().GetBool("json")
	allProjects, _ := cmd.Flags().GetBool("all-projects")

	// initConfig resolves the actual cwd via ResolveCwd (the --cwd flag, or
	// os.Getwd()), the same way doctor.go/models.go do — not
	// cfg.WorkingDir() off an empty-string config.Load(), which never
	// matches the absolute paths real sessions record as project_path.
	cwd, _, err := initConfig(cmd, false)
	if err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}
	event.StatsViewed()

	queries, _, cleanup, err := connectDB(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// The aggregation itself lives in internal/stats, shared with the
	// TUI's /stats screen, so the two can never disagree about the same
	// numbers. This command's job is the flags, the scope they select,
	// and the tables.
	snap, err := gather.Gather(ctx, queries, stats.Request{
		Scope: stats.ScopeProject,
		// cwd, not cfg.WorkingDir(): sessions record project_path as an
		// absolute path, which an empty-string config.Load() never
		// matches. Same reasoning as doctor.go/models.go.
		ProjectPath: cwd,
		Since:       since,
		WithSkills:  by == "" || by == "skills",
		// Latency is not part of the default view: it answers a
		// question about this build's internals, not about what the
		// user spent, and putting it in every `sennit stat` would push
		// the numbers people actually came for off the top of a screen.
		WithLatency: by == "latency",
	})
	if err != nil {
		return err
	}

	out := statOutput{Since: sinceFlag}
	if by == "" || by == "models" {
		out.Models = snap.Models
	}
	if by == "" || by == "agents" {
		out.Agents = snap.Agents
	}
	if by == "" || by == "projects" {
		if by == "projects" && allProjects {
			global, err := gather.Gather(ctx, queries, stats.Request{Scope: stats.ScopeGlobal, Since: since})
			if err != nil {
				return err
			}
			out.Projects = global.Projects
		} else {
			out.Projects = []stats.Project{snap.Totals}
		}
	}
	if by == "" || by == "skills" {
		out.Skills = snap.Skills
	}
	if by == "latency" {
		out.Latency = snap.Latency
	}
	if by == "" {
		summary := snap.Totals
		out.Summary = &summary
	}

	w := cmd.OutOrStdout()
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	renderStatTables(w, by, out)
	return nil
}

// renderStatTables prints the requested sections as aligned terminal
// tables via text/tabwriter.
func renderStatTables(w io.Writer, by string, out statOutput) {
	if by == "" || by == "models" {
		fmt.Fprintln(w, "MODELS")
		printModelsTable(w, out.Models)
		fmt.Fprintln(w)
	}
	if by == "" || by == "agents" {
		fmt.Fprintln(w, "AGENTS")
		printAgentsTable(w, out.Agents)
		fmt.Fprintln(w)
	}
	if by == "" || by == "projects" {
		fmt.Fprintln(w, "PROJECTS")
		printProjectsTable(w, out.Projects)
		fmt.Fprintln(w)
	}
	if by == "" || by == "skills" {
		fmt.Fprintln(w, "SKILLS")
		printSkillsTable(w, out.Skills)
		fmt.Fprintln(w)
	}
	if by == "latency" {
		fmt.Fprintln(w, "LATENCY")
		printLatencyTable(w, out.Latency)
		fmt.Fprintln(w)
	}
	if out.Summary != nil {
		fmt.Fprintln(w, "SUMMARY")
		fmt.Fprintf(w, "Total sessions: %s\n", humanize.Comma(out.Summary.Sessions))
		fmt.Fprintf(w, "Total tokens:   %s\n", humanize.Comma(out.Summary.PromptTokens+out.Summary.CompletionTokens))
		fmt.Fprintf(w, "Total cost:     $%.4f\n", out.Summary.Cost)
		fmt.Fprintf(w, "Total time:     %s\n", formatStatDuration(out.Summary.TimeSeconds))
	}
}

func printModelsTable(w io.Writer, models []stats.Model) {
	if len(models) == 0 {
		fmt.Fprintln(w, "No model usage recorded in this period.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tPROVIDER\tMESSAGES\tTIME\tPROMPT\tCOMPLETION\tCOST")
	hasApprox := false
	for _, m := range models {
		mark := ""
		if m.Approximate {
			mark = "~"
			hasApprox = true
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s%s\t%s%s\t%s%.4f\n",
			m.Model, m.Provider, humanize.Comma(m.MessageCount), formatStatDuration(m.TimeSeconds),
			mark, humanize.Comma(m.PromptTokens), mark, humanize.Comma(m.CompletionTokens), mark, m.Cost)
	}
	_ = tw.Flush()
	if hasApprox {
		fmt.Fprintln(w, "~ approximate: tokens/cost split proportionally across models used in mixed-model sessions")
	}
}

func printAgentsTable(w io.Writer, agents []stats.Agent) {
	if len(agents) == 0 {
		fmt.Fprintln(w, "No subagent runs recorded in this period.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tRUNS\tTIME\tPROMPT\tCOMPLETION\tCOST")
	for _, a := range agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%.4f\n",
			a.Name, humanize.Comma(a.Runs), formatStatDuration(a.TimeSeconds),
			humanize.Comma(a.PromptTokens), humanize.Comma(a.CompletionTokens), a.Cost)
	}
	_ = tw.Flush()
}

func printProjectsTable(w io.Writer, projects []stats.Project) {
	if len(projects) == 0 {
		fmt.Fprintln(w, "No sessions recorded in this period.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tSESSIONS\tTIME\tPROMPT\tCOMPLETION\tCOST")
	for _, p := range projects {
		path := p.Path
		if path == "" {
			path = "(current)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%.4f\n",
			path, humanize.Comma(p.Sessions), formatStatDuration(p.TimeSeconds),
			humanize.Comma(p.PromptTokens), humanize.Comma(p.CompletionTokens), p.Cost)
	}
	_ = tw.Flush()
}

func printSkillsTable(w io.Writer, skills []stats.Skill) {
	if len(skills) == 0 {
		fmt.Fprintln(w, "No skill loads recorded in this period.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SKILL\tLOADS\tSESSIONS\tFIRST USED\tLAST USED")
	for _, s := range skills {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.Name, humanize.Comma(s.LoadCount), humanize.Comma(s.SessionCount), s.FirstUsedAt, s.LastUsedAt)
	}
	_ = tw.Flush()
}

// printLatencyTable prints the per-kind handoff latency breakdown.
func printLatencyTable(w io.Writer, rows []stats.Latency) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No handoff latency recorded in this period.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tEVENTS\tP50\tP95\tMAX")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Kind, humanize.Comma(r.Events),
			formatStatMillis(r.P50MS), formatStatMillis(r.P95MS), formatStatMillis(r.MaxMS))
	}
	_ = tw.Flush()
}

// formatStatMillis renders a millisecond count the way a reader scans a
// latency column: milliseconds stay milliseconds, and only once a value
// crosses a second does it get time.Duration's compact form. Reusing
// formatStatDuration here would round every one of these to "0s".
func formatStatMillis(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}

// formatStatDuration renders a duration in seconds as a compact string
// (e.g. "1h2m3s"). go-humanize has no duration humanizer, so this rounds
// to the second and reuses time.Duration's own formatting.
func formatStatDuration(seconds int64) string {
	return (time.Duration(seconds) * time.Second).String()
}
