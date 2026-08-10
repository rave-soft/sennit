package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rave-soft/braid/internal/config"
	braiddb "github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/event"
	"github.com/spf13/cobra"
)

// statCmd prints usage breakdowns by model, agent, project, and skill
// straight to the terminal, in tokens and time, for use in scripts and
// quick checks. It absorbed the older `stats` (plural) command, which
// rendered an HTML dashboard and opened it in a browser; `stats` is kept
// only as an alias for muscle memory, and `stat` never touches a browser.
var statCmd = &cobra.Command{
	Use:     "stat",
	Aliases: []string{"stats"},
	Short:   "Show usage statistics as terminal tables",
	Long: `Show usage statistics as terminal tables, broken down by model,
subagent, project, and skill, in tokens and time. Also available as
"braid stats" for backwards compatibility.

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
    bucket, while custom agents (defined in config) get their own name.`,
	RunE: runStat,
}

func init() {
	statCmd.Flags().String("by", "", "Show only one section: models, agents, projects, or skills (default: all)")
	statCmd.Flags().String("since", "30d", "Time window: 7d, 30d, or all")
	statCmd.Flags().Bool("json", false, "Output machine-readable JSON instead of tables")
	statCmd.Flags().Bool("all-projects", false, "With --by projects, aggregate across every known project")
}

// statSince resolves the --since flag to a unix timestamp lower bound.
// "all" resolves to 0 (the unix epoch), which predates any real Braid
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

// statModelKey identifies a (model, provider) pair.
type statModelKey struct {
	model    string
	provider string
}

// statModel is one row of the models breakdown.
type statModel struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	MessageCount     int64   `json:"message_count"`
	TimeSeconds      int64   `json:"time_seconds"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
	// Approximate is true when this row's tokens/cost were split
	// proportionally across models within a mixed-model session, rather
	// than attributed from a single-model session exactly.
	Approximate bool `json:"approximate"`
}

// statAgent is one row of the subagent breakdown, grouped by session
// title (see the command's Long help for why title is the best available
// proxy for agent name).
type statAgent struct {
	Name             string  `json:"name"`
	Runs             int64   `json:"runs"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
	TimeSeconds      int64   `json:"time_seconds"`
}

// statProject is one row of the project breakdown (or the summary's
// current-project total, or a per-project row under --all-projects).
type statProject struct {
	Path             string  `json:"path"`
	Sessions         int64   `json:"sessions"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
	TimeSeconds      int64   `json:"time_seconds"`
}

// statSkill is one row of the skill-usage breakdown.
type statSkill struct {
	Name         string `json:"name"`
	LoadCount    int64  `json:"load_count"`
	SessionCount int64  `json:"session_count"`
	FirstUsedAt  string `json:"first_used_at,omitempty"`
	LastUsedAt   string `json:"last_used_at,omitempty"`
}

// statOutput is the top-level shape for --json output. Only the sections
// actually requested via --by are populated.
type statOutput struct {
	Since    string        `json:"since"`
	Models   []statModel   `json:"models,omitempty"`
	Agents   []statAgent   `json:"agents,omitempty"`
	Projects []statProject `json:"projects,omitempty"`
	Skills   []statSkill   `json:"skills,omitempty"`
	Summary  *statProject  `json:"summary,omitempty"`
}

func runStat(cmd *cobra.Command, _ []string) error {
	// cmd.Context() is nil unless the command was dispatched through
	// Execute(); fall back to Background() so RunE also works when
	// invoked directly (as tests do). See the same pattern in models.go.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	by, _ := cmd.Flags().GetString("by")
	switch by {
	case "", "models", "agents", "projects", "skills":
	default:
		return fmt.Errorf("invalid --by value %q: must be one of models, agents, projects, skills", by)
	}

	sinceFlag, _ := cmd.Flags().GetString("since")
	since, err := statSince(sinceFlag)
	if err != nil {
		return err
	}

	jsonOut, _ := cmd.Flags().GetBool("json")
	allProjects, _ := cmd.Flags().GetBool("all-projects")

	// Resolve the actual cwd via ResolveCwd (the --cwd flag, or
	// os.Getwd()), the same way doctor.go/models.go do — not
	// cfg.WorkingDir() off an empty-string config.Init(), which never
	// matches the absolute paths real sessions record as project_path.
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	dataDir, _ := cmd.Flags().GetString("data-dir")
	if _, err := config.Init(cwd, dataDir, false); err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}
	event.StatsViewed()

	conn, err := braiddb.Connect(ctx, config.GlobalDBDir())
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer braiddb.Release(config.GlobalDBDir()) //nolint:errcheck // best-effort refcount release on exit
	queries := braiddb.New(conn)

	// projectPath scopes the default (non --all-projects) view to the
	// current project, now that every project shares one DB file.
	projectPath := cwd

	sessions, err := queries.ListSessionsSince(ctx, braiddb.ListSessionsSinceParams{CreatedAt: since, ProjectPath: projectPath})
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}
	messages, err := queries.ListAssistantMessagesSince(ctx, braiddb.ListAssistantMessagesSinceParams{CreatedAt: since, ProjectPath: projectPath})
	if err != nil {
		return fmt.Errorf("failed to list assistant messages: %w", err)
	}

	out := statOutput{Since: sinceFlag}

	if by == "" || by == "models" {
		out.Models = computeModelStats(sessions, messages)
	}
	if by == "" || by == "agents" {
		out.Agents = computeAgentStats(sessions)
	}
	if by == "" || by == "projects" {
		if by == "projects" && allProjects {
			out.Projects, err = gatherAllProjectStats(ctx, queries, since)
			if err != nil {
				return fmt.Errorf("failed to gather stats from projects: %w", err)
			}
		} else {
			out.Projects = []statProject{currentProjectStat(sessions)}
		}
	}
	if by == "" || by == "skills" {
		skillRows, err := queries.ListSkillLoadsSince(ctx, braiddb.ListSkillLoadsSinceParams{CreatedAt: since, ProjectPath: projectPath})
		if err != nil {
			return fmt.Errorf("failed to list skill loads: %w", err)
		}
		out.Skills = computeSkillStats(skillRows)
	}
	if by == "" {
		summary := currentProjectStat(sessions)
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

// computeModelStats groups assistant messages by (model, provider) for
// exact message counts and time, then attributes each session's tokens
// and cost to the model(s) its assistant messages used — exactly for
// single-model sessions, proportionally (and marked Approximate) for
// sessions that mixed models.
func computeModelStats(sessions []braiddb.ListSessionsSinceRow, messages []braiddb.ListAssistantMessagesSinceRow) []statModel {
	messagesBySession := make(map[string][]braiddb.ListAssistantMessagesSinceRow)
	timeByModel := make(map[statModelKey]int64)
	countByModel := make(map[statModelKey]int64)
	for _, m := range messages {
		key := statModelKey{model: m.Model, provider: m.Provider}
		timeByModel[key] += m.FinishedAt - m.CreatedAt
		countByModel[key]++
		messagesBySession[m.SessionID] = append(messagesBySession[m.SessionID], m)
	}

	type tokenTotals struct {
		prompt     int64
		completion int64
		cost       float64
	}
	tokensByModel := make(map[statModelKey]tokenTotals)
	approxByModel := make(map[statModelKey]bool)

	for _, s := range sessions {
		msgs := messagesBySession[s.ID]
		if len(msgs) == 0 {
			continue
		}
		countPerModel := make(map[statModelKey]int64)
		for _, m := range msgs {
			countPerModel[statModelKey{model: m.Model, provider: m.Provider}]++
		}

		if len(countPerModel) == 1 {
			for key := range countPerModel {
				t := tokensByModel[key]
				t.prompt += s.PromptTokens
				t.completion += s.CompletionTokens
				t.cost += s.Cost
				tokensByModel[key] = t
			}
			continue
		}

		var total int64
		for _, c := range countPerModel {
			total += c
		}
		for key, c := range countPerModel {
			share := float64(c) / float64(total)
			t := tokensByModel[key]
			t.prompt += int64(math.Round(float64(s.PromptTokens) * share))
			t.completion += int64(math.Round(float64(s.CompletionTokens) * share))
			t.cost += s.Cost * share
			tokensByModel[key] = t
			approxByModel[key] = true
		}
	}

	keys := make(map[statModelKey]bool)
	for k := range countByModel {
		keys[k] = true
	}
	for k := range tokensByModel {
		keys[k] = true
	}

	result := make([]statModel, 0, len(keys))
	for k := range keys {
		t := tokensByModel[k]
		result = append(result, statModel{
			Model:            k.model,
			Provider:         k.provider,
			MessageCount:     countByModel[k],
			TimeSeconds:      timeByModel[k],
			PromptTokens:     t.prompt,
			CompletionTokens: t.completion,
			Cost:             t.cost,
			Approximate:      approxByModel[k],
		})
	}
	sort.Slice(result, func(i, j int) bool {
		ti := result[i].PromptTokens + result[i].CompletionTokens
		tj := result[j].PromptTokens + result[j].CompletionTokens
		if ti != tj {
			return ti > tj
		}
		return result[i].Model < result[j].Model
	})
	return result
}

// computeAgentStats groups subagent sessions (parent_session_id set) by
// title. Unlike model attribution, this is exact: each subagent session
// has its own prompt_tokens/completion_tokens/cost, and its wall-clock
// duration is updated_at - created_at.
func computeAgentStats(sessions []braiddb.ListSessionsSinceRow) []statAgent {
	byTitle := make(map[string]*statAgent)
	var order []string
	for _, s := range sessions {
		if !s.ParentSessionID.Valid || s.ParentSessionID.String == "" {
			continue
		}
		a, ok := byTitle[s.Title]
		if !ok {
			a = &statAgent{Name: s.Title}
			byTitle[s.Title] = a
			order = append(order, s.Title)
		}
		a.Runs++
		a.PromptTokens += s.PromptTokens
		a.CompletionTokens += s.CompletionTokens
		a.Cost += s.Cost
		a.TimeSeconds += s.UpdatedAt - s.CreatedAt
	}

	result := make([]statAgent, 0, len(order))
	for _, title := range order {
		result = append(result, *byTitle[title])
	}
	sort.Slice(result, func(i, j int) bool {
		ti := result[i].PromptTokens + result[i].CompletionTokens
		tj := result[j].PromptTokens + result[j].CompletionTokens
		if ti != tj {
			return ti > tj
		}
		return result[i].Name < result[j].Name
	})
	return result
}

// currentProjectStat aggregates top-level sessions (parent_session_id
// empty) into a single totals row, matching the scope `braid stats` uses
// for its own totals.
func currentProjectStat(sessions []braiddb.ListSessionsSinceRow) statProject {
	var p statProject
	for _, s := range sessions {
		if s.ParentSessionID.Valid && s.ParentSessionID.String != "" {
			continue
		}
		p.Sessions++
		p.PromptTokens += s.PromptTokens
		p.CompletionTokens += s.CompletionTokens
		p.Cost += s.Cost
		p.TimeSeconds += s.UpdatedAt - s.CreatedAt
	}
	return p
}

// gatherAllProjectStats aggregates one row per project known to the shared
// DB (like `braid stats --all`), plus a trailing totals row. Now that every
// project shares one DB file, this is a single GROUP BY query rather than a
// walk over each project's own (no longer existing) database file.
func gatherAllProjectStats(ctx context.Context, queries *braiddb.Queries, since int64) ([]statProject, error) {
	dbRows, err := queries.ProjectStatsSince(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("failed to gather project stats: %w", err)
	}

	rows := make([]statProject, 0, len(dbRows))
	var totals statProject
	totals.Path = "TOTAL"

	for _, r := range dbRows {
		promptTokens, _ := statCoerceInt64(r.PromptTokens)
		completionTokens, _ := statCoerceInt64(r.CompletionTokens)
		timeSeconds, _ := statCoerceInt64(r.TimeSeconds)
		cost, _ := statCoerceFloat64(r.Cost)

		row := statProject{
			Path:             r.ProjectPath,
			Sessions:         r.Sessions,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			Cost:             cost,
			TimeSeconds:      timeSeconds,
		}
		rows = append(rows, row)

		totals.Sessions += row.Sessions
		totals.PromptTokens += row.PromptTokens
		totals.CompletionTokens += row.CompletionTokens
		totals.Cost += row.Cost
		totals.TimeSeconds += row.TimeSeconds
	}

	// ProjectStatsSince already orders by (prompt_tokens +
	// completion_tokens) DESC; sort again in Go to match the sibling
	// gathering functions in this file and stay correct regardless of the
	// SQL ORDER BY.
	sort.Slice(rows, func(i, j int) bool {
		ti := rows[i].PromptTokens + rows[i].CompletionTokens
		tj := rows[j].PromptTokens + rows[j].CompletionTokens
		return ti > tj
	})

	return append(rows, totals), nil
}

// computeSkillStats converts raw ListSkillLoadsSince rows (whose
// aggregate columns come back as interface{} because sqlc can't infer a
// static type across the json_each/json_extract join) into typed rows.
func computeSkillStats(rows []braiddb.ListSkillLoadsSinceRow) []statSkill {
	result := make([]statSkill, 0, len(rows))
	for _, r := range rows {
		name, ok := r.SkillName.(string)
		if !ok || name == "" {
			continue
		}
		s := statSkill{
			Name:         name,
			LoadCount:    r.LoadCount,
			SessionCount: r.SessionCount,
		}
		if v, ok := statCoerceInt64(r.FirstUsedAt); ok {
			s.FirstUsedAt = time.Unix(v, 0).Format(time.RFC3339)
		}
		if v, ok := statCoerceInt64(r.LastUsedAt); ok {
			s.LastUsedAt = time.Unix(v, 0).Format(time.RFC3339)
		}
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].LoadCount != result[j].LoadCount {
			return result[i].LoadCount > result[j].LoadCount
		}
		return result[i].Name < result[j].Name
	})
	return result
}

// statCoerceInt64 coerces a database/sql/driver scan result to int64. The
// SQLite drivers used here (modernc.org/sqlite, ncruces/go-sqlite3) both
// return int64 for INTEGER aggregates, but this stays defensive since the
// column arrives as interface{}.
func statCoerceInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// statCoerceFloat64 coerces a database/sql/driver scan result to float64,
// for aggregate columns (like ProjectStatsSince's SUM(cost)) that arrive as
// interface{} because sqlc can't infer a static type through COALESCE.
func statCoerceFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
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
	if out.Summary != nil {
		fmt.Fprintln(w, "SUMMARY")
		fmt.Fprintf(w, "Total sessions: %s\n", humanize.Comma(out.Summary.Sessions))
		fmt.Fprintf(w, "Total tokens:   %s\n", humanize.Comma(out.Summary.PromptTokens+out.Summary.CompletionTokens))
		fmt.Fprintf(w, "Total cost:     $%.4f\n", out.Summary.Cost)
		fmt.Fprintf(w, "Total time:     %s\n", formatStatDuration(out.Summary.TimeSeconds))
	}
}

func printModelsTable(w io.Writer, models []statModel) {
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

func printAgentsTable(w io.Writer, agents []statAgent) {
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

func printProjectsTable(w io.Writer, projects []statProject) {
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

func printSkillsTable(w io.Writer, skills []statSkill) {
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

// formatStatDuration renders a duration in seconds as a compact string
// (e.g. "1h2m3s"). go-humanize has no duration humanizer, so this rounds
// to the second and reuses time.Duration's own formatting.
func formatStatDuration(seconds int64) string {
	return (time.Duration(seconds) * time.Second).String()
}
