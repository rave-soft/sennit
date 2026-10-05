package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/permission"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
)

// TestStepContext_ReadIsHeldToWhatTheContextHasLeft drives the two halves
// of the context budget through the turn the way a run does: a step
// finishes, leaving its usage and a large tool result behind, and the next
// step's tools get a context that knows how little room that leaves.
//
// The window is 100k and its summarize buffer 20k. The finished step used
// 70k and returned a 40KB tool result, about 10k tokens that no usage
// figure has counted, so nothing is free and a read gets the minimum grant.
// Counting the usage alone would leave 10k tokens and grant 15KB; no budget
// at all would return the whole 150KB file.
func TestStepContext_ReadIsHeldToWhatTheContextHasLeft(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "sess")
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	sessions := sessionstore.NewService(q, conn, "/test/project")
	sess, err := sessions.Create(ctx, "context budget")
	require.NoError(t, err)
	assistant, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)

	turn := &runTurn{
		agent:            &sessionAgent{sessions: sessions, messages: messages},
		ctx:              ctx,
		genCtx:           ctx,
		currentAssistant: &assistant,
		currentSession:   sess,
		call:             SessionAgentCall{SessionID: sess.ID},
		model:            Model{CatalogCfg: catwalk.Model{ContextWindow: 100_000}},
	}
	require.NoError(t, turn.onStepFinish(fantasy.StepResult{
		Response: fantasy.Response{
			FinishReason: fantasy.FinishReasonToolCalls,
			Usage:        fantasy.Usage{InputTokens: 69_000, OutputTokens: 1_000},
			Content: fantasy.ResponseContent{fantasy.ToolResultContent{
				ToolCallID: "call-0",
				ToolName:   tools.ReadToolName,
				Result:     fantasy.ToolResultOutputContentText{Text: strings.Repeat("x", 40_000)},
			}},
		},
	}))

	stepCtx, err := turn.createStepAssistant(ctx, nil)
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat(strings.Repeat("y", 99)+"\n", 1500)), 0o644))
	readTool := tools.NewReadTool(nil, permission.NewPermissionService(dir, false, nil), newFileTracking(fakeFileTracker{}), nil, dir)
	input, err := json.Marshal(tools.ReadParams{FilePath: path})
	require.NoError(t, err)

	resp, err := readTool.Run(stepCtx, fantasy.ToolCall{ID: "call-1", Name: tools.ReadToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	var meta tools.ReadResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.True(t, meta.Truncated)
	require.LessOrEqual(t, len(meta.Content), 8*1024)
	require.Contains(t, resp.Content, "context window is nearly full")
}
