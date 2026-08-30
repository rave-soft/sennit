package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/session"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/stretchr/testify/require"
)

// failCreateOnce wraps a real message store and fails the first Create
// call whose tool result answers one of the given call IDs — standing in
// for a transient SQLite busy on exactly one synthetic write, the case
// the seal-skip guards against.
type failCreateOnce struct {
	messagestore.Service
	failFor map[string]struct{}
}

func (f *failCreateOnce) Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	for _, p := range params.Parts {
		if tr, ok := p.(message.ToolResult); ok {
			if _, fail := f.failFor[tr.ToolCallID]; fail {
				delete(f.failFor, tr.ToolCallID)
				return message.Message{}, errors.New("injected create failure")
			}
		}
	}
	return f.Service.Create(ctx, sessionID, params)
}

// interruptedEnv is a project's session and message services over a real
// database, which this needs: the sweep's candidate query is SQL (an
// assistant message with a null finished_at, joined to its project), so a
// fake would be testing the fake.
type interruptedEnv struct {
	sessions sessionstore.Service
	messages messagestore.Service
}

// interruptedTestProject is the project every session in these tests
// belongs to. The sweep is scoped by project, so the tests need a second
// name to pass in as "somebody else's" - see
// TestFinalizeInterruptedTurns_IgnoresOtherProjects.
const (
	interruptedTestProject = "/proj"
	otherTestProject       = "/other-proj"
)

func newInterruptedEnv(t *testing.T) interruptedEnv {
	t.Helper()
	projectPath := interruptedTestProject
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := db.New(conn)
	return interruptedEnv{
		sessions: sessionstore.NewService(q, conn, projectPath),
		messages: messagestore.NewService(q),
	}
}

// assistantWithToolCall creates a session holding an assistant message
// with one unfinished tool call and no result — the shape a killed
// process leaves behind.
func (e interruptedEnv) assistantWithToolCall(t *testing.T) (session.Session, message.Message) {
	t.Helper()
	const callID = "call-1"
	sess, err := e.sessions.Create(t.Context(), "s")
	require.NoError(t, err)
	msg, err := e.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID: callID, Name: "agent", Input: "{}", Finished: false,
		}},
	})
	require.NoError(t, err)
	return sess, msg
}

func (e interruptedEnv) reload(t *testing.T, sessionID, messageID string) message.Message {
	t.Helper()
	require.NoError(t, e.messages.FlushAll(t.Context()))
	msgs, err := e.messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	for _, m := range msgs {
		if m.ID == messageID {
			return m
		}
	}
	t.Fatalf("message %s not found", messageID)
	return message.Message{}
}

// The whole point: a tool call a killed process never answered gets an
// error result and its turn gets a Finish, which is what stops the UI
// reading it as still running (chat.ToolRenderOpts.IsPending).
func TestFinalizeInterruptedTurns_AnswersDanglingToolCall(t *testing.T) {
	env := newInterruptedEnv(t)
	sess, msg := env.assistantWithToolCall(t)

	require.NoError(t, finalizeInterruptedTurns(t.Context(), interruptedTestProject, env.messages))

	got := env.reload(t, sess.ID, msg.ID)
	require.NotNil(t, got.FinishPart(), "the turn must be closed out")
	require.Equal(t, message.FinishReasonCanceled, got.FinishReason())

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var results []message.ToolResult
	for _, m := range msgs {
		if m.Role == message.Tool {
			results = append(results, m.ToolResults()...)
		}
	}
	require.Len(t, results, 1, "the unanswered call must get exactly one result")
	require.Equal(t, "call-1", results[0].ToolCallID)
	require.True(t, results[0].IsError, "a call that never came back did not succeed")
}

// A turn that ended normally is not this sweep's business, and rewriting
// it would corrupt real history.
func TestFinalizeInterruptedTurns_LeavesFinishedTurnsAlone(t *testing.T) {
	env := newInterruptedEnv(t)
	sess, msg := env.assistantWithToolCall(t)
	msg.AddFinish(message.FinishReasonEndTurn, time.Now().Unix(), "", "")
	require.NoError(t, env.messages.Update(t.Context(), msg))
	require.NoError(t, env.messages.FlushAll(t.Context()))

	require.NoError(t, finalizeInterruptedTurns(t.Context(), interruptedTestProject, env.messages))

	got := env.reload(t, sess.ID, msg.ID)
	require.Equal(t, message.FinishReasonEndTurn, got.FinishReason(),
		"a finished turn keeps the reason it actually finished with")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotEqual(t, message.Tool, m.Role, "no result may be invented for a finished turn")
	}
}

// Parallel tool calls can be killed with one answered and one not. Only
// the missing one is written; the recorded result must survive untouched.
func TestFinalizeInterruptedTurns_OnlyAnswersTheMissingCall(t *testing.T) {
	env := newInterruptedEnv(t)
	sess, err := env.sessions.Create(t.Context(), "s")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "done", Name: "bash", Input: "{}", Finished: true},
			message.ToolCall{ID: "dangling", Name: "agent", Input: "{}", Finished: false},
		},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "done", Name: "bash", Content: "real output",
		}},
	})
	require.NoError(t, err)

	require.NoError(t, finalizeInterruptedTurns(t.Context(), interruptedTestProject, env.messages))

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	byID := map[string]message.ToolResult{}
	for _, m := range msgs {
		for _, tr := range m.ToolResults() {
			require.NotContains(t, byID, tr.ToolCallID, "a call must not be answered twice")
			byID[tr.ToolCallID] = tr
		}
	}
	require.Len(t, byID, 2)
	require.Equal(t, "real output", byID["done"].Content, "a recorded result must survive")
	require.False(t, byID["done"].IsError)
	require.True(t, byID["dangling"].IsError)
}

// The sweep runs per project, and the database is shared by all of them.
// Another project's interrupted turn is another bootstrap's job — sweeping
// it here would repair sessions belonging to a sennit that may be running.
func TestFinalizeInterruptedTurns_IgnoresOtherProjects(t *testing.T) {
	env := newInterruptedEnv(t)
	sess, msg := env.assistantWithToolCall(t)

	require.NoError(t, finalizeInterruptedTurns(t.Context(), otherTestProject, env.messages))

	got := env.reload(t, sess.ID, msg.ID)
	require.Nil(t, got.FinishPart(), "another project's turn must be left for its own bootstrap")
}

// A failed synthetic write must not be papered over with a seal: the
// comment on finalizeInterruptedTurns promises the message is "left to be
// retried on the next start rather than sealed half-repaired", and this
// is what makes that true instead of aspirational.
func TestFinalizeInterruptedTurns_LeavesMessageUnsealedOnCreateFailure(t *testing.T) {
	env := newInterruptedEnv(t)
	sess, msg := env.assistantWithToolCall(t)
	failing := &failCreateOnce{Service: env.messages, failFor: map[string]struct{}{"call-1": {}}}

	require.NoError(t, finalizeInterruptedTurns(t.Context(), interruptedTestProject, failing))

	got := env.reload(t, sess.ID, msg.ID)
	require.Nil(t, got.FinishPart(), "a half-repaired message must not be sealed")

	unfinished, err := env.messages.ListUnfinishedAssistantMessages(t.Context(), interruptedTestProject)
	require.NoError(t, err)
	require.Len(t, unfinished, 1, "the message must still be found on the next start")
	require.Equal(t, msg.ID, unfinished[0].ID)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotEqual(t, message.Tool, m.Role, "the failed write must not have partially landed")
	}
}

// A retriable failure on one message's tool call must not stop the sweep
// from repairing everything else it found.
func TestFinalizeInterruptedTurns_OneFailureDoesNotStopTheRest(t *testing.T) {
	env := newInterruptedEnv(t)
	_, failedMsg := env.assistantWithToolCall(t)
	sess2, err := env.sessions.Create(t.Context(), "s2")
	require.NoError(t, err)
	okMsg, err := env.messages.Create(t.Context(), sess2.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID: "call-2", Name: "agent", Input: "{}", Finished: false,
		}},
	})
	require.NoError(t, err)
	failing := &failCreateOnce{Service: env.messages, failFor: map[string]struct{}{"call-1": {}}}

	require.NoError(t, finalizeInterruptedTurns(t.Context(), interruptedTestProject, failing))

	gotFailed := env.reload(t, failedMsg.SessionID, failedMsg.ID)
	require.Nil(t, gotFailed.FinishPart(), "the message whose write failed stays unsealed")

	gotOK := env.reload(t, sess2.ID, okMsg.ID)
	require.NotNil(t, gotOK.FinishPart(), "the other message must still be repaired and sealed")
	require.Equal(t, message.FinishReasonCanceled, gotOK.FinishReason())
}

// Running twice must be a no-op the second time: the Finish written by the
// first pass is what takes the message out of the candidate query.
func TestFinalizeInterruptedTurns_IsIdempotent(t *testing.T) {
	env := newInterruptedEnv(t)
	sess, _ := env.assistantWithToolCall(t)

	require.NoError(t, finalizeInterruptedTurns(t.Context(), interruptedTestProject, env.messages))
	require.NoError(t, env.messages.FlushAll(t.Context()))
	require.NoError(t, finalizeInterruptedTurns(t.Context(), interruptedTestProject, env.messages))
	require.NoError(t, env.messages.FlushAll(t.Context()))

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var results int
	for _, m := range msgs {
		results += len(m.ToolResults())
	}
	require.Equal(t, 1, results, "a second sweep must not answer the same call again")
}
