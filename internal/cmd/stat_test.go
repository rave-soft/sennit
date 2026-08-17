package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	sennitdb "github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// testProjectPath is the project_path statFixture seeds its "current
// project" sessions under, for tests that query sennitdb.Queries directly
// rather than going through runStat (so no --cwd resolution is involved —
// any fixed placeholder path works, it just has to match what's passed to
// the Since queries in the same test).
const testProjectPath = "/fake/project"

// newStatTestCmd builds a minimal cobra.Command carrying the flags
// statCmd.RunE reads, mirroring newRefreshTestCmd in models_test.go.
// Tests invoke RunE directly rather than going through rootCmd.Execute()
// to keep them hermetic.
func newStatTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	testCmd := &cobra.Command{Use: "stat"}
	testCmd.Flags().StringP("cwd", "c", "", "")
	testCmd.Flags().StringP("data-dir", "D", "", "")
	testCmd.Flags().String("by", "", "")
	testCmd.Flags().String("since", "30d", "")
	testCmd.Flags().Bool("json", false, "")
	testCmd.Flags().Bool("all-projects", false, "")

	var stdout bytes.Buffer
	testCmd.SetOut(&stdout)
	testCmd.SetArgs(nil)
	return testCmd, &stdout
}

// statFixture opens a fresh migrated DB and seeds it with
// sessions/messages exercising: multiple distinct (model, provider)
// pairs, a mixed-model session (to exercise the proportional-split
// approximate path), a generic-task subagent session, a custom-agent
// subagent session, a skill load, and a row that predates the --since
// cutoff (to verify period filtering). dir is the directory to open the
// database in; an empty dir gets a fresh temp dir of its own. Tests that
// exercise runStat (which now reads from the shared global DB directory,
// not --data-dir) must pass config.GlobalDBDir() so the fixture lands
// where runStat will actually look. projectPath is the project_path every
// seeded session is stamped with; callers that exercise runStat must pass
// whatever ResolveCwd will actually resolve to (see the --cwd flag set on
// the test command) so the scoping filter matches.
func statFixture(t *testing.T, dir, projectPath string) string {
	t.Helper()
	dataDir := dir
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	t.Cleanup(func() {
		require.NoError(t, sennitdb.Release(dataDir))
		sennitdb.ResetPool()
	})

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	ctx := t.Context()

	now := time.Now()
	recent := now.Add(-1 * time.Hour).Unix()
	old := now.Add(-40 * 24 * time.Hour).Unix() // outside a 7d or 30d window

	// setSessionTimes and setMessageTimes bypass the CreateSession/
	// CreateMessage strftime('now') defaults so fixtures can straddle the
	// --since cutoff.
	setSessionTimes := func(id string, createdAt, updatedAt int64) {
		_, err := conn.ExecContext(ctx, `UPDATE sessions SET created_at = ?, updated_at = ? WHERE id = ?`, createdAt, updatedAt, id)
		require.NoError(t, err)
	}
	setMessageTimes := func(id string, createdAt, finishedAt int64) {
		_, err := conn.ExecContext(ctx, `UPDATE messages SET created_at = ?, finished_at = ? WHERE id = ?`, createdAt, finishedAt, id)
		require.NoError(t, err)
	}

	// Session A: single model throughout (anthropic/claude), exact
	// attribution expected.
	sessA, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{
		ID: "sess-a", Title: "session A", ProjectPath: projectPath,
		PromptTokens: 1000, CompletionTokens: 500, Cost: 1.5,
	})
	require.NoError(t, err)
	setSessionTimes(sessA.ID, recent, recent+120)
	msgA1, err := q.CreateMessage(ctx, sennitdb.CreateMessageParams{
		ID: "msg-a1", SessionID: sessA.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "claude-sonnet", Valid: true},
		Provider: sql.NullString{String: "anthropic", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgA1.ID, recent, recent+60)

	// Session B: a different single model (openai/gpt), exact
	// attribution, distinct (model, provider) pair from session A.
	sessB, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{
		ID: "sess-b", Title: "session B", ProjectPath: projectPath,
		PromptTokens: 2000, CompletionTokens: 800, Cost: 2.0,
	})
	require.NoError(t, err)
	setSessionTimes(sessB.ID, recent, recent+200)
	msgB1, err := q.CreateMessage(ctx, sennitdb.CreateMessageParams{
		ID: "msg-b1", SessionID: sessB.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "gpt-5", Valid: true},
		Provider: sql.NullString{String: "openai", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgB1.ID, recent, recent+90)

	// Session C: mixed models (2x claude-sonnet, 1x gpt-5 assistant
	// messages) -- exercises the proportional-split/approximate path.
	sessC, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{
		ID: "sess-c", Title: "session C", ProjectPath: projectPath,
		PromptTokens: 900, CompletionTokens: 300, Cost: 3.0,
	})
	require.NoError(t, err)
	setSessionTimes(sessC.ID, recent, recent+300)
	msgC1, err := q.CreateMessage(ctx, sennitdb.CreateMessageParams{
		ID: "msg-c1", SessionID: sessC.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "claude-sonnet", Valid: true},
		Provider: sql.NullString{String: "anthropic", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgC1.ID, recent, recent+30)
	msgC2, err := q.CreateMessage(ctx, sennitdb.CreateMessageParams{
		ID: "msg-c2", SessionID: sessC.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "claude-sonnet", Valid: true},
		Provider: sql.NullString{String: "anthropic", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgC2.ID, recent, recent+30)
	msgC3, err := q.CreateMessage(ctx, sennitdb.CreateMessageParams{
		ID: "msg-c3", SessionID: sessC.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "gpt-5", Valid: true},
		Provider: sql.NullString{String: "openai", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgC3.ID, recent, recent+30)

	// Subagent session delegated via the generic "task" tool: title is
	// always "New Agent Session".
	sessTask, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{
		ID: "sess-task", ParentSessionID: sql.NullString{String: sessA.ID, Valid: true}, ProjectPath: projectPath,
		Title: "New Agent Session", PromptTokens: 300, CompletionTokens: 100, Cost: 0.4,
	})
	require.NoError(t, err)
	setSessionTimes(sessTask.ID, recent, recent+50)

	// Subagent session delegated via a custom agent: title is the
	// configured agent name.
	sessCustom, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{
		ID: "sess-custom", ParentSessionID: sql.NullString{String: sessA.ID, Valid: true}, ProjectPath: projectPath,
		Title: "reviewer", PromptTokens: 500, CompletionTokens: 200, Cost: 0.6,
	})
	require.NoError(t, err)
	setSessionTimes(sessCustom.ID, recent, recent+80)

	// A skill load: a tool_result message part shaped like the `view`
	// tool's response when it reads a skill file.
	meta := tools.ReadResponseMetadata{
		ResourceType: tools.ReadResourceSkill,
		ResourceName: "some-skill",
	}
	metaJSON, err := json.Marshal(meta)
	require.NoError(t, err)
	partsJSON, err := message.MarshalParts([]message.ContentPart{
		message.ToolResult{ToolCallID: "tc-1", Name: "view", Content: "skill contents", Metadata: string(metaJSON)},
	})
	require.NoError(t, err)
	msgSkill, err := q.CreateMessage(ctx, sennitdb.CreateMessageParams{
		ID: "msg-skill", SessionID: sessA.ID, Role: "tool", Parts: string(partsJSON),
	})
	require.NoError(t, err)
	setMessageTimes(msgSkill.ID, recent, recent)

	// A session entirely outside the --since window: must be excluded
	// once filtering is applied.
	sessOld, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{
		ID: "sess-old", Title: "ancient session", ProjectPath: projectPath,
		PromptTokens: 9999, CompletionTokens: 9999, Cost: 99,
	})
	require.NoError(t, err)
	setSessionTimes(sessOld.ID, old, old+10)
	msgOld, err := q.CreateMessage(ctx, sennitdb.CreateMessageParams{
		ID: "msg-old", SessionID: sessOld.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "claude-sonnet", Valid: true},
		Provider: sql.NullString{String: "anthropic", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgOld.ID, old, old+10)

	return dataDir
}

// otherProjectPath is a second project_path, distinct from testProjectPath,
// used by tests that must prove project scoping doesn't leak sessions
// across projects sharing the one DB.
const otherProjectPath = "/other/project"

// seedOtherProjectSession adds a single top-level session under
// otherProjectPath to the shared DB at dataDir, alongside whatever
// statFixture already seeded there. Used by tests asserting that
// project-scoped queries and gatherAllProjectStats keep this project's
// totals separate from the fixture's own.
func seedOtherProjectSession(t *testing.T, dataDir string) {
	t.Helper()
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	ctx := t.Context()

	recent := time.Now().Add(-1 * time.Hour).Unix()
	sess, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{
		ID: "sess-other", Title: "other project session", ProjectPath: otherProjectPath,
		PromptTokens: 400, CompletionTokens: 150, Cost: 0.75,
	})
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET created_at = ?, updated_at = ? WHERE id = ?`, recent, recent+70, sess.ID)
	require.NoError(t, err)
}

func TestComputeModelStats_ExactAndApproximateAttribution(t *testing.T) {
	dataDir := statFixture(t, "", testProjectPath)
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)

	since, err := statSince("7d")
	require.NoError(t, err)
	snap, err := stats.Gather(t.Context(), q, stats.Request{
		Scope: stats.ScopeProject, ProjectPath: testProjectPath, Since: since,
	})
	require.NoError(t, err)
	models := snap.Models
	require.Len(t, models, 2, "expected exactly the two distinct (model, provider) pairs, old session excluded")

	byModel := make(map[string]stats.Model)
	for _, m := range models {
		byModel[m.Model] = m
	}

	claude, ok := byModel["claude-sonnet"]
	require.True(t, ok)
	require.Equal(t, "anthropic", claude.Provider)
	// 1 exact message from session A + 2 mixed-session messages from
	// session C.
	require.Equal(t, int64(3), claude.MessageCount)
	require.True(t, claude.Approximate, "claude-sonnet also appears in mixed-model session C, so its tokens are a split estimate")
	// Exact from session A (1000/500) + 2/3 share of session C's
	// 900/300.
	wantPrompt := int64(1000) + int64(600) // round(900 * 2/3)
	wantCompletion := int64(500) + int64(200)
	require.Equal(t, wantPrompt, claude.PromptTokens)
	require.Equal(t, wantCompletion, claude.CompletionTokens)

	gpt, ok := byModel["gpt-5"]
	require.True(t, ok)
	require.Equal(t, "openai", gpt.Provider)
	require.Equal(t, int64(2), gpt.MessageCount)
	require.True(t, gpt.Approximate)
	wantGPTPrompt := int64(2000) + int64(300) // round(900 * 1/3)
	wantGPTCompletion := int64(800) + int64(100)
	require.Equal(t, wantGPTPrompt, gpt.PromptTokens)
	require.Equal(t, wantGPTCompletion, gpt.CompletionTokens)
}

func TestComputeAgentStats_GroupsByTitle(t *testing.T) {
	dataDir := statFixture(t, "", testProjectPath)
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)

	since, err := statSince("7d")
	require.NoError(t, err)
	snap, err := stats.Gather(t.Context(), q, stats.Request{
		Scope: stats.ScopeProject, ProjectPath: testProjectPath, Since: since,
	})
	require.NoError(t, err)
	agents := snap.Agents
	require.Len(t, agents, 2)

	byName := make(map[string]stats.Agent)
	for _, a := range agents {
		byName[a.Name] = a
	}

	task, ok := byName["New Agent Session"]
	require.True(t, ok, "generic task-tool delegations should collapse into the 'New Agent Session' bucket")
	require.Equal(t, int64(1), task.Runs)
	require.Equal(t, int64(300), task.PromptTokens)
	require.Equal(t, int64(100), task.CompletionTokens)

	reviewer, ok := byName["reviewer"]
	require.True(t, ok, "custom agents should be grouped under their configured name")
	require.Equal(t, int64(1), reviewer.Runs)
	require.Equal(t, int64(500), reviewer.PromptTokens)
	require.Equal(t, int64(200), reviewer.CompletionTokens)
}

func TestComputeSkillStats_MatchesDoubleJSONExtract(t *testing.T) {
	dataDir := statFixture(t, "", testProjectPath)
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)

	since, err := statSince("7d")
	require.NoError(t, err)
	rows, err := q.ListSkillLoadsSince(t.Context(), sennitdb.ListSkillLoadsSinceParams{CreatedAt: since, ProjectPath: testProjectPath})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the skill-loading tool_result message must be matched by the double json_extract query")

	snap, err := stats.Gather(t.Context(), q, stats.Request{
		Scope: stats.ScopeProject, ProjectPath: testProjectPath, Since: since, WithSkills: true,
	})
	require.NoError(t, err)
	skills := snap.Skills
	require.Len(t, skills, 1)
	require.Equal(t, "some-skill", skills[0].Name)
	require.Equal(t, int64(1), skills[0].LoadCount)
	require.Equal(t, int64(1), skills[0].SessionCount)
	require.NotEmpty(t, skills[0].FirstUsedAt)
}

func TestComputeSessionStats_SinceFiltersOutOldSessions(t *testing.T) {
	dataDir := statFixture(t, "", testProjectPath)
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)

	sinceAll, err := statSince("all")
	require.NoError(t, err)
	allSessions, err := q.ListSessionsSince(t.Context(), sennitdb.ListSessionsSinceParams{CreatedAt: sinceAll, ProjectPath: testProjectPath})
	require.NoError(t, err)
	require.Contains(t, titlesOf(allSessions), "ancient session")

	since7d, err := statSince("7d")
	require.NoError(t, err)
	recentSessions, err := q.ListSessionsSince(t.Context(), sennitdb.ListSessionsSinceParams{CreatedAt: since7d, ProjectPath: testProjectPath})
	require.NoError(t, err)
	require.NotContains(t, titlesOf(recentSessions), "ancient session")
}

func titlesOf(sessions []sennitdb.ListSessionsSinceRow) []string {
	titles := make([]string, len(sessions))
	for i, s := range sessions {
		titles[i] = s.Title
	}
	return titles
}

func TestStatCmd_JSONOutput(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	// runStat now reads the shared global DB, not --data-dir, so the
	// fixture must be seeded there for the command to see it. runStat
	// scopes to the project resolved by ResolveCwd (the --cwd flag set
	// below), so the fixture's sessions must be seeded under that same
	// path.
	cwd := t.TempDir()
	dataDir := statFixture(t, config.GlobalDBDir(), cwd)

	testCmd, stdout := newStatTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", cwd))
	require.NoError(t, testCmd.Flags().Set("data-dir", dataDir))
	require.NoError(t, testCmd.Flags().Set("since", "7d"))
	require.NoError(t, testCmd.Flags().Set("json", "true"))

	require.NoError(t, runStat(testCmd, nil))

	var out statOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))

	require.Len(t, out.Models, 2)
	require.Len(t, out.Agents, 2)
	require.Len(t, out.Projects, 1)
	require.Len(t, out.Skills, 1)
	require.NotNil(t, out.Summary)

	// Summary scopes to top-level sessions only (sessA + sessB + sessC),
	// excluding subagent sessions and the out-of-window sessOld.
	require.Equal(t, int64(3), out.Summary.Sessions)
	require.Equal(t, int64(1000+500+2000+800+900+300), out.Summary.PromptTokens+out.Summary.CompletionTokens)
}

func TestStatCmd_InvalidByIsUsageError(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	// runStat now reads the shared global DB, not --data-dir, so the
	// fixture must be seeded there for the command to see it.
	cwd := t.TempDir()
	dataDir := statFixture(t, config.GlobalDBDir(), cwd)

	testCmd, _ := newStatTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", cwd))
	require.NoError(t, testCmd.Flags().Set("data-dir", dataDir))
	require.NoError(t, testCmd.Flags().Set("by", "bogus"))

	err := runStat(testCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --by value")
}

func TestStatCmd_InvalidSinceIsUsageError(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	// runStat now reads the shared global DB, not --data-dir, so the
	// fixture must be seeded there for the command to see it.
	cwd := t.TempDir()
	dataDir := statFixture(t, config.GlobalDBDir(), cwd)

	testCmd, _ := newStatTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", cwd))
	require.NoError(t, testCmd.Flags().Set("data-dir", dataDir))
	require.NoError(t, testCmd.Flags().Set("since", "bogus"))

	err := runStat(testCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --since value")
}

// TestGatherAllProjectStats_GroupsByProjectWithoutLeaking is the acceptance
// test for step 3 of the shared-DB migration: with two projects' sessions
// in the one shared DB, gatherAllProjectStats must report one row per
// project_path with correct per-project totals, via a single GROUP BY
// query rather than the old per-file walk.
func TestGatherAllProjectStats_GroupsByProjectWithoutLeaking(t *testing.T) {
	dataDir := statFixture(t, "", testProjectPath)
	seedOtherProjectSession(t, dataDir)

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)

	since, err := statSince("7d")
	require.NoError(t, err)

	global, err := stats.Gather(t.Context(), q, stats.Request{Scope: stats.ScopeGlobal, Since: since})
	require.NoError(t, err)
	rows := global.Projects
	require.Len(t, rows, 3, "testProjectPath row, otherProjectPath row, and a trailing TOTAL row")

	byPath := make(map[string]stats.Project)
	for _, r := range rows {
		byPath[r.Path] = r
	}

	current, ok := byPath[testProjectPath]
	require.True(t, ok)
	// Top-level sessions only (sessA + sessB + sessC): sessTask/sessCustom
	// are subagent sessions (parent_session_id set) and excluded, sessOld
	// is outside the 7d window and excluded.
	require.Equal(t, int64(3), current.Sessions)
	require.Equal(t, int64(1000+2000+900), current.PromptTokens)
	require.Equal(t, int64(500+800+300), current.CompletionTokens)
	require.InDelta(t, 1.5+2.0+3.0, current.Cost, 0.0001)

	other, ok := byPath[otherProjectPath]
	require.True(t, ok)
	require.Equal(t, int64(1), other.Sessions)
	require.Equal(t, int64(400), other.PromptTokens)
	require.Equal(t, int64(150), other.CompletionTokens)
	require.InDelta(t, 0.75, other.Cost, 0.0001)

	total := rows[len(rows)-1]
	require.Equal(t, "TOTAL", total.Path)
	require.Equal(t, current.Sessions+other.Sessions, total.Sessions)
	require.Equal(t, current.PromptTokens+other.PromptTokens, total.PromptTokens)
	require.Equal(t, current.CompletionTokens+other.CompletionTokens, total.CompletionTokens)
}

// TestRunStat_DefaultProjectsViewScopedToCurrentProject is the acceptance
// test for the regression fix: the default (non --all-projects) `--by
// projects` view must only report the current project's own sessions, even
// when the shared DB holds another project's sessions too.
func TestRunStat_DefaultProjectsViewScopedToCurrentProject(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	// runStat resolves "current project" via ResolveCwd (the --cwd flag set
	// below), so statFixture's sessions must be seeded under that same
	// path for runStat to report them; seedOtherProjectSession's session
	// (under the distinct otherProjectPath) must not leak in.
	cwd := t.TempDir()
	dataDir := statFixture(t, config.GlobalDBDir(), cwd)
	seedOtherProjectSession(t, config.GlobalDBDir())

	testCmd, stdout := newStatTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", cwd))
	require.NoError(t, testCmd.Flags().Set("data-dir", dataDir))
	require.NoError(t, testCmd.Flags().Set("since", "7d"))
	require.NoError(t, testCmd.Flags().Set("by", "projects"))
	require.NoError(t, testCmd.Flags().Set("json", "true"))

	require.NoError(t, runStat(testCmd, nil))

	var out statOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))

	require.Len(t, out.Projects, 1)
	require.Equal(t, int64(3), out.Projects[0].Sessions)
	require.Equal(t, int64(1000+2000+900), out.Projects[0].PromptTokens)
	require.Equal(t, int64(500+800+300), out.Projects[0].CompletionTokens)
}

// seedLatencyEvents records handoff waits against one of the fixture's
// sessions, so they inherit its project_path and land inside the window
// the tests query. It writes through the recorder rather than raw SQL to
// keep the write path under test alongside the read path.
func seedLatencyEvents(t *testing.T, dataDir string, waits map[latency.Kind][]time.Duration) {
	t.Helper()
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	rec := latency.NewService(sennitdb.New(conn))
	for kind, list := range waits {
		for _, w := range list {
			rec.Record(t.Context(), kind, "sess-a", w)
		}
	}
}

// The latency section is opt-in, and asking for it must not also drag in
// the sections it replaces — the point of --by is that one table is what
// you get.
func TestStatCmd_LatencySectionIsOptIn(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	dataDir := statFixture(t, config.GlobalDBDir(), cwd)
	seedLatencyEvents(t, dataDir, map[latency.Kind][]time.Duration{
		latency.KindSteeringFold:       {10 * time.Millisecond, 20 * time.Millisecond, 2 * time.Second},
		latency.KindCompletionDelivery: {5 * time.Millisecond},
	})

	// Default view: no latency, and none of its query's cost paid.
	defaultCmd, _ := newStatTestCmd(t)
	require.NoError(t, defaultCmd.Flags().Set("cwd", cwd))
	require.NoError(t, defaultCmd.Flags().Set("data-dir", dataDir))
	require.NoError(t, defaultCmd.Flags().Set("json", "true"))
	require.NoError(t, runStat(defaultCmd, nil))
	var defaultOut statOutput
	require.NoError(t, json.Unmarshal(defaultCmd.OutOrStdout().(*bytes.Buffer).Bytes(), &defaultOut))
	require.Empty(t, defaultOut.Latency, "latency must not appear unless asked for")

	testCmd, stdout := newStatTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", cwd))
	require.NoError(t, testCmd.Flags().Set("data-dir", dataDir))
	require.NoError(t, testCmd.Flags().Set("by", "latency"))
	require.NoError(t, testCmd.Flags().Set("json", "true"))
	require.NoError(t, runStat(testCmd, nil))

	var out statOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Empty(t, out.Models, "--by latency shows only latency")
	require.Empty(t, out.Skills)
	require.Nil(t, out.Summary)

	require.Len(t, out.Latency, 2)
	byKind := map[string]stats.Latency{}
	for _, row := range out.Latency {
		byKind[row.Kind] = row
	}
	steering := byKind["steering_fold"]
	require.Equal(t, int64(3), steering.Events)
	require.Equal(t, int64(20), steering.P50MS)
	require.Equal(t, int64(2000), steering.MaxMS, "the outlier survives into the table rather than averaging away")
	require.Equal(t, int64(1), byKind["completion_delivery"].Events)
}

// Sub-second waits are the normal case, and rendering them through the
// seconds-granularity duration formatter would print every one of them
// as "0s".
func TestStatCmd_LatencyTableKeepsMillisecondResolution(t *testing.T) {
	var buf bytes.Buffer
	printLatencyTable(&buf, []stats.Latency{
		{Kind: "steering_fold", Events: 2, P50MS: 12, P95MS: 900, MaxMS: 2500},
	})

	out := buf.String()
	require.Contains(t, out, "12ms")
	require.Contains(t, out, "900ms")
	require.Contains(t, out, "2.5s", "past a second the compact duration form is easier to read")
	require.NotContains(t, out, "0s")
}

func TestStatCmd_LatencyTableSaysSoWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	printLatencyTable(&buf, nil)
	require.Contains(t, buf.String(), "No handoff latency recorded")
}
