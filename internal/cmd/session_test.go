package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	sennitdb "github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// newSessionTestCmd builds a minimal cobra.Command carrying the flags
// sessionSetup/ResolveCwd read, matching the pattern in stat_test.go and
// gc_test.go: tests invoke RunE directly rather than going through
// rootCmd.Execute() to keep them hermetic. "json" is registered here too,
// since the five session subcommands now read --json off the invoked
// cobra.Command's own flag set (see sessionSetJSON below) rather than a
// package-level bool.
func newSessionTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	testCmd := &cobra.Command{Use: "session"}
	testCmd.Flags().StringP("cwd", "c", "", "")
	testCmd.Flags().StringP("data-dir", "D", "", "")
	testCmd.Flags().Bool("json", false, "")

	var stdout bytes.Buffer
	testCmd.SetOut(&stdout)
	testCmd.SetArgs(nil)
	return testCmd, &stdout
}

// sessionSetJSON sets cmd's --json flag to true, for tests exercising the
// JSON output path of a runSession* command.
func sessionSetJSON(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	require.NoError(t, cmd.Flags().Set("json", "true"))
}

// sessionFixtureIDs names every session sessionFixture seeds, so tests can
// assert on exactly which ones a command acted on.
type sessionFixtureIDs struct {
	Older string // top-level session, "Session Alpha", oldest updated_at
	Newer string // top-level session, "Session Beta", most recently updated -> "last"
	Child string // agent-tool child session of Older, must never appear in list/last
}

// sessionFixture opens a fresh migrated DB at dataDir (config.GlobalDBDir()
// for tests that exercise the session subcommands, since sessionSetup
// always connects there regardless of --data-dir) and seeds it with two
// top-level sessions plus one agent-tool child session, going through
// session.Service/message.Service the same way sessionSetup does rather
// than raw SQL, so the fixture exercises the real write paths the CLI
// commands read back through. projectPath must be whatever ResolveCwd will
// resolve --cwd to, so the project-scoped List/GetLast queries see these
// rows.
func sessionFixture(t *testing.T, dataDir, projectPath string) sessionFixtureIDs {
	t.Helper()
	t.Cleanup(func() {
		require.NoError(t, sennitdb.Release(dataDir))
		sennitdb.ResetPool()
	})

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	ctx := t.Context()

	sessSvc := session.NewService(q, conn, projectPath)
	msgSvc := message.NewService(q)

	older, err := sessSvc.Create(ctx, "Session Alpha")
	require.NoError(t, err)
	newer, err := sessSvc.Create(ctx, "Session Beta")
	require.NoError(t, err)
	child, err := sessSvc.CreateTaskSession(ctx, "toolcall-child-1", older.ID, "New Agent Session")
	require.NoError(t, err)

	_, err = msgSvc.Create(ctx, older.ID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    []message.ContentPart{message.TextContent{Text: "hello from alpha"}},
		Model:    "claude-sonnet",
		Provider: "anthropic",
	})
	require.NoError(t, err)

	// Backdate explicitly and last, matching the convention in
	// gcFixture/statFixture: the message insert above bumps Older's
	// updated_at via the session's row-touching trigger, so any timestamp
	// control has to happen after every other write to that row.
	now := time.Now()
	setTime := func(id string, at time.Time) {
		_, err := conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ?, created_at = ? WHERE id = ?`, at.Unix(), at.Unix(), id)
		require.NoError(t, err)
	}
	setTime(older.ID, now.Add(-2*time.Hour))
	setTime(newer.ID, now.Add(-1*time.Hour))
	setTime(child.ID, now.Add(-90*time.Minute))

	return sessionFixtureIDs{Older: older.ID, Newer: newer.ID, Child: child.ID}
}

// sessionRowExists reports whether a session with the given id is still in
// the database at dataDir.
func sessionRowExists(t *testing.T, dataDir, id string) bool {
	t.Helper()
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	defer sennitdb.Release(dataDir) //nolint:errcheck
	q := sennitdb.New(conn)
	_, err = q.GetSessionByID(t.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	require.NoError(t, err)
	return true
}

// sessionTitle reads back a session's current title.
func sessionTitle(t *testing.T, dataDir, id string) string {
	t.Helper()
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	defer sennitdb.Release(dataDir) //nolint:errcheck
	q := sennitdb.New(conn)
	row, err := q.GetSessionByID(t.Context(), id)
	require.NoError(t, err)
	return row.Title
}

// captureStdout redirects the process's real os.Stdout to a pipe for the
// rest of the test, since outputSessionHuman/sessionWriter write straight
// to os.Stdout rather than through cmd.OutOrStdout() (they need the real
// file descriptor to detect terminal size and TTY-ness). The returned
// func restores os.Stdout and returns everything written to it. Because
// os.Stdout is process-global, callers of this helper cannot be
// t.Parallel() with anything else that writes to stdout.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	return func() string {
		os.Stdout = orig
		require.NoError(t, w.Close())
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		require.NoError(t, r.Close())
		return buf.String()
	}
}

// --- resolveSessionID ---

func TestResolveSessionID_FullUUIDResolves(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, sennitdb.Release(dataDir))
		sennitdb.ResetPool()
	})
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	svc := session.NewService(q, conn, "/proj")

	sess, err := svc.Create(t.Context(), "target")
	require.NoError(t, err)

	got, err := resolveSessionID(t.Context(), svc, sess.ID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, got.ID)
}

func TestResolveSessionID_UniqueHashPrefixResolves(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, sennitdb.Release(dataDir))
		sennitdb.ResetPool()
	})
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	svc := session.NewService(q, conn, "/proj")

	sess, err := svc.Create(t.Context(), "target")
	require.NoError(t, err)

	prefix := session.HashID(sess.ID)[:8]
	got, err := resolveSessionID(t.Context(), svc, prefix)
	require.NoError(t, err)
	require.Equal(t, sess.ID, got.ID)
}

func TestResolveSessionID_NotFound(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, sennitdb.Release(dataDir))
		sennitdb.ResetPool()
	})
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	svc := session.NewService(q, conn, "/proj")

	_, err = svc.Create(t.Context(), "target")
	require.NoError(t, err)

	_, err = resolveSessionID(t.Context(), svc, "does-not-exist")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// findHashCollision deterministically scans literal session IDs until two
// of them share a 1-hex-character session.HashID prefix (16 buckets, so a
// birthday-bound collision lands within a handful of draws), returning
// those two IDs and the shared prefix. Used to build an ambiguous-prefix
// fixture without depending on random UUIDs happening to collide.
func findHashCollision(t *testing.T) (id1, id2, prefix string) {
	t.Helper()
	seen := make(map[string]string)
	for i := range 10000 {
		id := fmt.Sprintf("sess-collide-%d", i)
		p := session.HashID(id)[:1]
		if other, ok := seen[p]; ok {
			return other, id, p
		}
		seen[p] = id
	}
	t.Fatal("failed to find a hash prefix collision")
	return "", "", ""
}

func TestResolveSessionID_AmbiguousPrefixReportsMatches(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, sennitdb.Release(dataDir))
		sennitdb.ResetPool()
	})
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	ctx := t.Context()

	// resolveSessionID falls back to svc.List, which only returns
	// top-level sessions, so both colliding rows must be top-level
	// (parent_session_id IS NULL) with a controlled, literal ID -- which
	// is why these are seeded directly through sennitdb rather than
	// through session.Service.Create (which always assigns a random UUID).
	id1, id2, prefix := findHashCollision(t)
	_, err = q.CreateSession(ctx, sennitdb.CreateSessionParams{ID: id1, Title: "one", ProjectPath: "/proj"})
	require.NoError(t, err)
	_, err = q.CreateSession(ctx, sennitdb.CreateSessionParams{ID: id2, Title: "two", ProjectPath: "/proj"})
	require.NoError(t, err)

	svc := session.NewService(q, conn, "/proj")
	_, err = resolveSessionID(ctx, svc, prefix)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ambiguous")
	require.Contains(t, err.Error(), "one")
	require.Contains(t, err.Error(), "two")
}

// --- runSessionList ---

func TestRunSessionList_JSON_ListsOnlyTopLevelSessions(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, cwd) })
	ids := sessionFixture(t, config.GlobalDBDir(), cwd)

	testCmd, stdout := newSessionTestCmd(t)
	sessionSetJSON(t, testCmd)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionList(testCmd, nil))

	var out []sessionJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Len(t, out, 2, "the agent-tool child session must not appear in the list")

	var titles []string
	for _, s := range out {
		titles = append(titles, s.Title)
		require.NotEqual(t, ids.Child, s.UUID)
	}
	require.Contains(t, titles, "Session Alpha")
	require.Contains(t, titles, "Session Beta")
}

func TestRunSessionList_EmptyDatabase(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()

	testCmd, stdout := newSessionTestCmd(t)
	sessionSetJSON(t, testCmd)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionList(testCmd, nil))

	var out []sessionJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Empty(t, out)
}

func TestRunSessionList_Human_ContainsTitles(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	sessionFixture(t, config.GlobalDBDir(), cwd)

	restore := captureStdout(t)
	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionList(testCmd, nil))
	out := restore()

	require.Contains(t, out, "Session Alpha")
	require.Contains(t, out, "Session Beta")
}

// --- runSessionShow ---

func TestRunSessionShow_JSON_MapsFields(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	ids := sessionFixture(t, config.GlobalDBDir(), cwd)

	testCmd, stdout := newSessionTestCmd(t)
	sessionSetJSON(t, testCmd)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionShow(testCmd, []string{ids.Older}))

	var out sessionShowOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Equal(t, ids.Older, out.Meta.UUID)
	require.Equal(t, session.HashID(ids.Older), out.Meta.ID)
	require.Equal(t, "Session Alpha", out.Meta.Title)
	require.Len(t, out.Messages, 1)
	require.Equal(t, "assistant", out.Messages[0].Role)
	require.Len(t, out.Messages[0].Parts, 1)
	require.Equal(t, "text", out.Messages[0].Parts[0].Type)
	require.Equal(t, "hello from alpha", out.Messages[0].Parts[0].Text)
}

func TestRunSessionShow_HashPrefixResolves(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	ids := sessionFixture(t, config.GlobalDBDir(), cwd)

	testCmd, stdout := newSessionTestCmd(t)
	sessionSetJSON(t, testCmd)
	setCwdFlag(t, testCmd, cwd)
	prefix := session.HashID(ids.Older)[:8]
	require.NoError(t, runSessionShow(testCmd, []string{prefix}))

	var out sessionShowOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Equal(t, ids.Older, out.Meta.UUID)
}

func TestRunSessionShow_NotFound(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	sessionFixture(t, config.GlobalDBDir(), cwd)

	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	err := runSessionShow(testCmd, []string{"does-not-exist"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestRunSessionShow_Human_ContainsTitleAndID(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	ids := sessionFixture(t, config.GlobalDBDir(), cwd)

	restore := captureStdout(t)
	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionShow(testCmd, []string{ids.Older}))
	out := restore()

	require.Contains(t, out, "Session Alpha")
	require.Contains(t, out, session.HashID(ids.Older)[:12])
}

// --- runSessionLast ---

func TestRunSessionLast_JSON_ReturnsMostRecentlyUpdated(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	ids := sessionFixture(t, config.GlobalDBDir(), cwd)

	testCmd, stdout := newSessionTestCmd(t)
	sessionSetJSON(t, testCmd)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionLast(testCmd, nil))

	var out sessionShowOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Equal(t, ids.Newer, out.Meta.UUID, "Newer has the most recent updated_at of the two top-level sessions")
}

func TestRunSessionLast_EmptyDatabase_Errors(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()

	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	err := runSessionLast(testCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no sessions found")
}

// --- runSessionDelete ---

func TestRunSessionDelete_RemovesOnlyTheNamedSession(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	dataDir := config.GlobalDBDir()
	ids := sessionFixture(t, dataDir, cwd)

	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionDelete(testCmd, []string{ids.Newer}))

	require.False(t, sessionRowExists(t, dataDir, ids.Newer))
	require.True(t, sessionRowExists(t, dataDir, ids.Older), "an unrelated sibling session must survive")
	require.True(t, sessionRowExists(t, dataDir, ids.Child), "a child of an untouched parent must survive")
}

func TestRunSessionDelete_DeletingParentCascadesToChild(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	dataDir := config.GlobalDBDir()
	ids := sessionFixture(t, dataDir, cwd)

	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionDelete(testCmd, []string{ids.Older}))

	require.False(t, sessionRowExists(t, dataDir, ids.Older))
	require.False(t, sessionRowExists(t, dataDir, ids.Child), "session.Service.Delete removes the whole subtree, not just the named row")
	require.True(t, sessionRowExists(t, dataDir, ids.Newer), "an unrelated sibling session must survive")
}

func TestRunSessionDelete_JSON(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	dataDir := config.GlobalDBDir()
	ids := sessionFixture(t, dataDir, cwd)

	testCmd, stdout := newSessionTestCmd(t)
	sessionSetJSON(t, testCmd)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionDelete(testCmd, []string{ids.Newer}))

	var out sessionMutationResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Equal(t, ids.Newer, out.UUID)
	require.Equal(t, "Session Beta", out.Title)
	require.True(t, out.Deleted)
	require.False(t, out.Renamed)
}

func TestRunSessionDelete_NotFound(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	dataDir := config.GlobalDBDir()
	ids := sessionFixture(t, dataDir, cwd)

	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	err := runSessionDelete(testCmd, []string{"does-not-exist"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	// Nothing else must have been touched by a failed resolve.
	require.True(t, sessionRowExists(t, dataDir, ids.Older))
	require.True(t, sessionRowExists(t, dataDir, ids.Newer))
}

// --- runSessionRename ---

func TestRunSessionRename_RenamesOnlyTheNamedSession(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	dataDir := config.GlobalDBDir()
	ids := sessionFixture(t, dataDir, cwd)

	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionRename(testCmd, []string{ids.Older, "new", "title"}))

	require.Equal(t, "new title", sessionTitle(t, dataDir, ids.Older))
	require.Equal(t, "Session Beta", sessionTitle(t, dataDir, ids.Newer), "an unrelated sibling session's title must survive")
}

func TestRunSessionRename_JSON(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	dataDir := config.GlobalDBDir()
	ids := sessionFixture(t, dataDir, cwd)

	testCmd, stdout := newSessionTestCmd(t)
	sessionSetJSON(t, testCmd)
	setCwdFlag(t, testCmd, cwd)
	require.NoError(t, runSessionRename(testCmd, []string{ids.Older, "renamed", "title"}))

	var out sessionMutationResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Equal(t, ids.Older, out.UUID)
	require.Equal(t, "renamed title", out.Title)
	require.True(t, out.Renamed)
	require.False(t, out.Deleted)
}

func TestRunSessionRename_NotFound(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	cwd := t.TempDir()
	sessionFixture(t, config.GlobalDBDir(), cwd)

	testCmd, _ := newSessionTestCmd(t)
	setCwdFlag(t, testCmd, cwd)
	err := runSessionRename(testCmd, []string{"does-not-exist", "title"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// --- outputSessionJSON / outputSessionHuman / helper functions ---

func TestOutputSessionJSON_MapsAllMetaAndMessageFields(t *testing.T) {
	sess := session.Session{
		ID: "sess-1", Title: "Demo", CreatedAt: 100, UpdatedAt: 200,
		Cost: 1.25, PromptTokens: 10, CompletionTokens: 5,
	}
	msgs := []*message.Message{
		{
			ID: "m1", Role: message.Assistant, CreatedAt: 150,
			Model: "claude-sonnet", Provider: "anthropic",
			Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, outputSessionJSON(&buf, sess, msgs))

	var out sessionShowOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	require.Equal(t, session.HashID("sess-1"), out.Meta.ID)
	require.Equal(t, "sess-1", out.Meta.UUID)
	require.Equal(t, "Demo", out.Meta.Title)
	require.InDelta(t, 1.25, out.Meta.Cost, 0.0001)
	require.Equal(t, int64(10), out.Meta.PromptTokens)
	require.Equal(t, int64(5), out.Meta.CompletionTokens)
	require.Equal(t, int64(15), out.Meta.TotalTokens)
	require.Len(t, out.Messages, 1)
	require.Equal(t, "m1", out.Messages[0].ID)
	require.Equal(t, "assistant", out.Messages[0].Role)
	require.Equal(t, "claude-sonnet", out.Messages[0].Model)
	require.Equal(t, "anthropic", out.Messages[0].Provider)
}

func TestOutputSessionHuman_ContainsTitleAndID(t *testing.T) {
	cwd := t.TempDir()
	cfg, err := config.Load(cwd, cwd, false)
	require.NoError(t, err)

	sess := session.Session{ID: "sess-1", Title: "Demo Session", CreatedAt: time.Now().Unix()}
	msgs := []*message.Message{
		{
			ID: "m1", Role: message.Assistant, CreatedAt: time.Now().Unix(),
			Parts: []message.ContentPart{message.TextContent{Text: "hi there"}},
		},
	}

	restore := captureStdout(t)
	require.NoError(t, outputSessionHuman(t.Context(), cfg, sess, msgs))
	out := restore()

	require.Contains(t, out, "Demo Session")
	require.Contains(t, out, session.HashID("sess-1")[:12])
}

func TestMessagePtrs(t *testing.T) {
	msgs := []message.Message{{ID: "a"}, {ID: "b"}}
	ptrs := messagePtrs(msgs)
	require.Len(t, ptrs, 2)
	require.Equal(t, "a", ptrs[0].ID)
	require.Equal(t, "b", ptrs[1].ID)
	// The pointers alias the backing slice, not copies.
	msgs[0].ID = "changed"
	require.Equal(t, "changed", ptrs[0].ID)
}

func TestConvertParts_MapsEveryPartType(t *testing.T) {
	parts := []message.ContentPart{
		message.TextContent{Text: "hi"},
		message.ReasoningContent{Thinking: "thinking...", StartedAt: 1, FinishedAt: 2},
		message.ToolCall{ID: "tc1", Name: "bash", Input: `{"cmd":"ls"}`},
		message.ToolResult{ToolCallID: "tc1", Name: "bash", Content: "out", IsError: true, MIMEType: "text/plain"},
		message.BinaryContent{MIMEType: "image/png", Data: []byte{1, 2, 3, 4}},
		message.ImageURLContent{URL: "http://example.com/x.png", Detail: "high"},
		message.Finish{Reason: message.FinishReasonEndTurn, Time: 42},
		message.ShellCommand{Command: "ls", Output: "out", ExitCode: 0},
	}

	out := convertParts(parts)
	require.Len(t, out, len(parts))

	require.Equal(t, "text", out[0].Type)
	require.Equal(t, "hi", out[0].Text)

	require.Equal(t, "reasoning", out[1].Type)
	require.Equal(t, "thinking...", out[1].Thinking)
	require.Equal(t, int64(1), out[1].StartedAt)
	require.Equal(t, int64(2), out[1].FinishedAt)

	require.Equal(t, "tool_call", out[2].Type)
	require.Equal(t, "tc1", out[2].ToolCallID)
	require.Equal(t, "bash", out[2].Name)
	require.Equal(t, `{"cmd":"ls"}`, out[2].Input)

	require.Equal(t, "tool_result", out[3].Type)
	require.Equal(t, "out", out[3].Content)
	require.True(t, out[3].IsError)
	require.Equal(t, "text/plain", out[3].MIMEType)

	require.Equal(t, "binary", out[4].Type)
	require.Equal(t, "image/png", out[4].MIMEType)
	require.Equal(t, int64(4), out[4].Size)

	require.Equal(t, "image_url", out[5].Type)
	require.Equal(t, "http://example.com/x.png", out[5].URL)
	require.Equal(t, "high", out[5].Detail)

	require.Equal(t, "finish", out[6].Type)
	require.Equal(t, "end_turn", out[6].Reason)
	require.Equal(t, int64(42), out[6].Time)

	require.Equal(t, "unknown", out[7].Type, "a part type convertParts has no case for must not be silently dropped")
}

func TestExtractSkillsFromMessages(t *testing.T) {
	skillMeta := `{"resource_type":"skill","resource_name":"my-skill","resource_description":"does things"}`
	msgs := []*message.Message{
		{
			ID: "m1", Role: message.Tool, CreatedAt: 1000,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "tc1", Name: "view", Content: "skill contents", Metadata: skillMeta},
			},
		},
		// A second load of the same skill must be deduplicated.
		{
			ID: "m2", Role: message.Tool, CreatedAt: 2000,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "tc2", Name: "view", Content: "skill contents again", Metadata: skillMeta},
			},
		},
	}

	skills := extractSkillsFromMessages(msgs)
	require.Len(t, skills, 1)
	require.Equal(t, "my-skill", skills[0].Name)
	require.Equal(t, "does things", skills[0].Description)
}

func TestExtractSkillsFromMessages_NoSkills(t *testing.T) {
	msgs := []*message.Message{
		{ID: "m1", Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}},
	}
	require.Empty(t, extractSkillsFromMessages(msgs))
}

// --- isBrokenPipe ---

func TestIsBrokenPipe(t *testing.T) {
	require.False(t, isBrokenPipe(nil))
	require.True(t, isBrokenPipe(syscall.EPIPE))
	require.True(t, isBrokenPipe(fmt.Errorf("write: %w", syscall.EPIPE)))
	require.True(t, isBrokenPipe(errors.New("write |1: broken pipe")))
	require.False(t, isBrokenPipe(errors.New("some other error")))
}

// --- sessionWriter ---

func TestSessionWriter_NonTerminalStdoutUsesPlainWriter(t *testing.T) {
	// os.Stdout in a test binary is never a TTY, so sessionWriter must take
	// the non-pager branch regardless of contentHeight.
	w, cleanup, usingPager := sessionWriter(t.Context(), 100000)
	defer cleanup()
	require.False(t, usingPager)
	require.NotNil(t, w)
}
