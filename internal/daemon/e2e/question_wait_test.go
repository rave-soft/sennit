package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/question"
)

// TestQuestionWaitsWithoutClients is TestPermissionWaitsWithoutClients's
// counterpart for the other half of CLIENT-SERVER-REVIEW.md's point 4: a
// pending question.Request has to hold the daemon busy exactly the same
// way a pending permission does, since the `question` tool (unlike bash,
// write, etc.) never goes through the permission service at all --
// idleBusyCheck.busy has a separate branch for it (internal/daemon/
// idle.go), and this is that branch's only exercise through the real
// binary and a real dropped/reconnected client.
func TestQuestionWaitsWithoutClients(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("daemon e2e: skipped under -race, see racecheck_off_test.go")
	}

	fixture := newFixtureServer(
		fixtureTurn{ToolCalls: []fixtureToolCall{{
			ID:   "call_1",
			Name: tools.QuestionToolName,
			Args: `{"questions":[{"type":"yes_no","question":"Proceed?","description":"Should I continue?"}]}`,
		}}},
		fixtureTurn{Text: "done after question"},
	)
	defer fixture.Close()

	projectDir := t.TempDir()
	writeShortIdleTimeoutConfig(t, projectDir)

	dp := startDaemonProcess(t, projectDir, fixture.URL)

	client, closeClient := dp.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()

	sess, err := client.CreateSession(ctx, "question wait test")
	require.NoError(t, err)
	require.NoError(t, client.AgentRun(ctx, sess.ID, "ask me something"))

	require.Eventually(t, func() bool {
		pending, err := client.PendingPrompts(ctx)
		return err == nil && len(pending.Questions) > 0
	}, hangGuard, 50*time.Millisecond, "question request never appeared")

	closeClient()

	time.Sleep(7 * time.Second)
	require.True(t, dp.stillRunning(t), "daemon exited while a question was still pending")

	client2, _ := dp.dial(t)
	pending, err := client2.PendingPrompts(ctx)
	require.NoError(t, err)
	require.Len(t, pending.Questions, 1)
	req := pending.Questions[0]
	require.Equal(t, sess.ID, req.SessionID)
	require.Len(t, req.Questions, 1)

	yes := true
	answered, err := client2.QuestionAnswer(req.ID, []question.Answer{{QuestionID: req.Questions[0].ID, Yes: &yes}})
	require.NoError(t, err)
	require.True(t, answered)

	require.Eventually(t, func() bool {
		return !client2.AgentIsSessionBusy(sess.ID)
	}, hangGuard, 50*time.Millisecond, "turn never finished after the question was answered")

	msgs, err := client2.ListMessages(ctx, sess.ID)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if m.Role == message.Assistant && m.Content().Text == "done after question" {
			found = true
		}
	}
	require.True(t, found, "expected the post-question assistant reply among %d messages", len(msgs))

	shutdownCleanly(t, dp, client2)
}
