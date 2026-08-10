package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/agent/tools"
	braiddb "github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/message"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// newStatTestCmd builds a minimal cobra.Command carrying the flags
// statCmd.RunE reads, mirroring newRefreshTestCmd in models_test.go.
// Tests invoke RunE directly rather than going through rootCmd.Execute()
// to keep them hermetic.
func newStatTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	testCmd := &cobra.Command{Use: "stat"}
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

// statFixture opens a fresh migrated DB in a temp dir and seeds it with
// sessions/messages exercising: multiple distinct (model, provider)
// pairs, a mixed-model session (to exercise the proportional-split
// approximate path), a generic-task subagent session, a custom-agent
// subagent session, a skill load, and a row that predates the --since
// cutoff (to verify period filtering).
func statFixture(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, braiddb.Release(dataDir))
		braiddb.ResetPool()
	})

	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := braiddb.New(conn)
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
	sessA, err := q.CreateSession(ctx, braiddb.CreateSessionParams{
		ID: "sess-a", Title: "session A",
		PromptTokens: 1000, CompletionTokens: 500, Cost: 1.5,
	})
	require.NoError(t, err)
	setSessionTimes(sessA.ID, recent, recent+120)
	msgA1, err := q.CreateMessage(ctx, braiddb.CreateMessageParams{
		ID: "msg-a1", SessionID: sessA.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "claude-sonnet", Valid: true},
		Provider: sql.NullString{String: "anthropic", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgA1.ID, recent, recent+60)

	// Session B: a different single model (openai/gpt), exact
	// attribution, distinct (model, provider) pair from session A.
	sessB, err := q.CreateSession(ctx, braiddb.CreateSessionParams{
		ID: "sess-b", Title: "session B",
		PromptTokens: 2000, CompletionTokens: 800, Cost: 2.0,
	})
	require.NoError(t, err)
	setSessionTimes(sessB.ID, recent, recent+200)
	msgB1, err := q.CreateMessage(ctx, braiddb.CreateMessageParams{
		ID: "msg-b1", SessionID: sessB.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "gpt-5", Valid: true},
		Provider: sql.NullString{String: "openai", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgB1.ID, recent, recent+90)

	// Session C: mixed models (2x claude-sonnet, 1x gpt-5 assistant
	// messages) -- exercises the proportional-split/approximate path.
	sessC, err := q.CreateSession(ctx, braiddb.CreateSessionParams{
		ID: "sess-c", Title: "session C",
		PromptTokens: 900, CompletionTokens: 300, Cost: 3.0,
	})
	require.NoError(t, err)
	setSessionTimes(sessC.ID, recent, recent+300)
	msgC1, err := q.CreateMessage(ctx, braiddb.CreateMessageParams{
		ID: "msg-c1", SessionID: sessC.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "claude-sonnet", Valid: true},
		Provider: sql.NullString{String: "anthropic", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgC1.ID, recent, recent+30)
	msgC2, err := q.CreateMessage(ctx, braiddb.CreateMessageParams{
		ID: "msg-c2", SessionID: sessC.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "claude-sonnet", Valid: true},
		Provider: sql.NullString{String: "anthropic", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgC2.ID, recent, recent+30)
	msgC3, err := q.CreateMessage(ctx, braiddb.CreateMessageParams{
		ID: "msg-c3", SessionID: sessC.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "gpt-5", Valid: true},
		Provider: sql.NullString{String: "openai", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgC3.ID, recent, recent+30)

	// Subagent session delegated via the generic "task" tool: title is
	// always "New Agent Session".
	sessTask, err := q.CreateSession(ctx, braiddb.CreateSessionParams{
		ID: "sess-task", ParentSessionID: sql.NullString{String: sessA.ID, Valid: true},
		Title: "New Agent Session", PromptTokens: 300, CompletionTokens: 100, Cost: 0.4,
	})
	require.NoError(t, err)
	setSessionTimes(sessTask.ID, recent, recent+50)

	// Subagent session delegated via a custom agent: title is the
	// configured agent name.
	sessCustom, err := q.CreateSession(ctx, braiddb.CreateSessionParams{
		ID: "sess-custom", ParentSessionID: sql.NullString{String: sessA.ID, Valid: true},
		Title: "reviewer", PromptTokens: 500, CompletionTokens: 200, Cost: 0.6,
	})
	require.NoError(t, err)
	setSessionTimes(sessCustom.ID, recent, recent+80)

	// A skill load: a tool_result message part shaped like the `view`
	// tool's response when it reads a skill file.
	meta := tools.ViewResponseMetadata{
		ResourceType: tools.ViewResourceSkill,
		ResourceName: "some-skill",
	}
	metaJSON, err := json.Marshal(meta)
	require.NoError(t, err)
	partsJSON, err := message.MarshalParts([]message.ContentPart{
		message.ToolResult{ToolCallID: "tc-1", Name: "view", Content: "skill contents", Metadata: string(metaJSON)},
	})
	require.NoError(t, err)
	msgSkill, err := q.CreateMessage(ctx, braiddb.CreateMessageParams{
		ID: "msg-skill", SessionID: sessA.ID, Role: "tool", Parts: string(partsJSON),
	})
	require.NoError(t, err)
	setMessageTimes(msgSkill.ID, recent, recent)

	// A session entirely outside the --since window: must be excluded
	// once filtering is applied.
	sessOld, err := q.CreateSession(ctx, braiddb.CreateSessionParams{
		ID: "sess-old", Title: "ancient session",
		PromptTokens: 9999, CompletionTokens: 9999, Cost: 99,
	})
	require.NoError(t, err)
	setSessionTimes(sessOld.ID, old, old+10)
	msgOld, err := q.CreateMessage(ctx, braiddb.CreateMessageParams{
		ID: "msg-old", SessionID: sessOld.ID, Role: "assistant", Parts: "[]",
		Model:    sql.NullString{String: "claude-sonnet", Valid: true},
		Provider: sql.NullString{String: "anthropic", Valid: true},
	})
	require.NoError(t, err)
	setMessageTimes(msgOld.ID, old, old+10)

	return dataDir
}

func TestComputeModelStats_ExactAndApproximateAttribution(t *testing.T) {
	dataDir := statFixture(t)
	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := braiddb.New(conn)

	since, err := statSince("7d")
	require.NoError(t, err)
	sessions, err := q.ListSessionsSince(t.Context(), since)
	require.NoError(t, err)
	messages, err := q.ListAssistantMessagesSince(t.Context(), since)
	require.NoError(t, err)

	models := computeModelStats(sessions, messages)
	require.Len(t, models, 2, "expected exactly the two distinct (model, provider) pairs, old session excluded")

	byModel := make(map[string]statModel)
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
	dataDir := statFixture(t)
	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := braiddb.New(conn)

	since, err := statSince("7d")
	require.NoError(t, err)
	sessions, err := q.ListSessionsSince(t.Context(), since)
	require.NoError(t, err)

	agents := computeAgentStats(sessions)
	require.Len(t, agents, 2)

	byName := make(map[string]statAgent)
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
	dataDir := statFixture(t)
	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := braiddb.New(conn)

	since, err := statSince("7d")
	require.NoError(t, err)
	rows, err := q.ListSkillLoadsSince(t.Context(), since)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the skill-loading tool_result message must be matched by the double json_extract query")

	skills := computeSkillStats(rows)
	require.Len(t, skills, 1)
	require.Equal(t, "some-skill", skills[0].Name)
	require.Equal(t, int64(1), skills[0].LoadCount)
	require.Equal(t, int64(1), skills[0].SessionCount)
	require.NotEmpty(t, skills[0].FirstUsedAt)
}

func TestComputeSessionStats_SinceFiltersOutOldSessions(t *testing.T) {
	dataDir := statFixture(t)
	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := braiddb.New(conn)

	sinceAll, err := statSince("all")
	require.NoError(t, err)
	allSessions, err := q.ListSessionsSince(t.Context(), sinceAll)
	require.NoError(t, err)
	require.Contains(t, titlesOf(allSessions), "ancient session")

	since7d, err := statSince("7d")
	require.NoError(t, err)
	recentSessions, err := q.ListSessionsSince(t.Context(), since7d)
	require.NoError(t, err)
	require.NotContains(t, titlesOf(recentSessions), "ancient session")
}

func titlesOf(sessions []braiddb.ListSessionsSinceRow) []string {
	titles := make([]string, len(sessions))
	for i, s := range sessions {
		titles[i] = s.Title
	}
	return titles
}

func TestStatCmd_JSONOutput(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := statFixture(t)

	testCmd, stdout := newStatTestCmd(t)
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
	dataDir := statFixture(t)

	testCmd, _ := newStatTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("data-dir", dataDir))
	require.NoError(t, testCmd.Flags().Set("by", "bogus"))

	err := runStat(testCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --by value")
}

func TestStatCmd_InvalidSinceIsUsageError(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := statFixture(t)

	testCmd, _ := newStatTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("data-dir", dataDir))
	require.NoError(t, testCmd.Flags().Set("since", "bogus"))

	err := runStat(testCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --since value")
}
