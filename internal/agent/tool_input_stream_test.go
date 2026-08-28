package agent

import (
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/stretchr/testify/require"
)

// TestOnToolInputDeltaPersistsArgumentsMidStream is the regression test for
// a turn that looks dead while it works.
//
// Only OnToolInputStart and OnToolCall used to be wired, so a call's
// arguments existed nowhere between the two: the message held a tool call
// with an empty Input until the whole thing had been generated. On a call
// whose arguments are the work - a write of a file composed in one go -
// that is minutes of an unchanged message, so nothing is published and the
// transcript shows a bare spinner with no sign of progress.
func TestOnToolInputDeltaPersistsArgumentsMidStream(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	// No debounce: this asserts on what a reader sees mid-stream, which
	// the coalescing window would otherwise hide behind a timer.
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	sessions := sessionstore.NewService(q, conn, "/test/project")

	sess, err := sessions.Create(ctx, "streaming arguments")
	require.NoError(t, err)
	assistant, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{},
	})
	require.NoError(t, err)

	turn := &runTurn{
		agent:            &sessionAgent{messages: messages},
		ctx:              ctx,
		genCtx:           ctx,
		currentAssistant: &assistant,
	}

	require.NoError(t, turn.onToolInputStart("tc-1", "write"))
	deltas := []string{`{"file_path":"/tmp/x.go",`, `"content":"package `, `main"}`}
	for _, delta := range deltas {
		require.NoError(t, turn.onToolInputDelta("tc-1", delta))
	}

	stored, err := messages.Get(ctx, assistant.ID)
	require.NoError(t, err)
	calls := stored.ToolCalls()
	require.Len(t, calls, 1)
	require.Equal(t, strings.Join(deltas, ""), calls[0].Input,
		"every delta must land in the persisted call, not just the finished input")
	require.False(t, calls[0].Finished,
		"arguments still streaming: only the end of the stream may finish the call")
}

// TestOnToolInputDeltaKeepsTheCallItGrows: the deltas belong to one call
// among several a step can open, and growing one must not disturb the rest
// or invent a call of its own.
func TestOnToolInputDeltaKeepsTheCallItGrows(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	sessions := sessionstore.NewService(q, conn, "/test/project")

	sess, err := sessions.Create(ctx, "parallel calls")
	require.NoError(t, err)
	assistant, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{},
	})
	require.NoError(t, err)

	turn := &runTurn{
		agent:            &sessionAgent{messages: messages},
		ctx:              ctx,
		genCtx:           ctx,
		currentAssistant: &assistant,
	}

	require.NoError(t, turn.onToolInputStart("tc-1", "write"))
	require.NoError(t, turn.onToolInputStart("tc-2", "bash"))
	require.NoError(t, turn.onToolInputDelta("tc-2", `{"command":"ls"}`))
	// A delta for a call that was never opened is a no-op, not a new call.
	require.NoError(t, turn.onToolInputDelta("tc-nope", `{"x":1}`))

	stored, err := messages.Get(ctx, assistant.ID)
	require.NoError(t, err)
	calls := stored.ToolCalls()
	require.Len(t, calls, 2)
	require.Equal(t, "tc-1", calls[0].ID)
	require.Empty(t, calls[0].Input, "an untouched call must not pick up another's arguments")
	require.Equal(t, "tc-2", calls[1].ID)
	require.Equal(t, `{"command":"ls"}`, calls[1].Input)
}
