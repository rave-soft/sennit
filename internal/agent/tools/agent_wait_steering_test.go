package tools

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestAgentWaitSteering(t *testing.T) {
	t.Parallel()
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", ParentSessionID: callerSession, Status: "running"}
	entered := make(chan struct{})
	exited := make(chan struct{})
	manager.wait = func(ctx context.Context, _ []string) error {
		close(entered)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}
	input := make(chan struct{})
	ctx := WithUserInput(context.WithValue(t.Context(), SessionIDContextKey, callerSession), func() <-chan struct{} { return input })
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	var response fantasy.ToolResponse
	var err error
	go func() {
		response, err = NewAgentWaitTool(manager, nil).Run(ctx, fantasy.ToolCall{ID: "wait", Input: `{"ids":["t1"]}`})
		close(done)
	}()
	<-entered
	close(input)
	<-done
	require.NoError(t, err)
	require.Contains(t, response.Content, "person sent a message")
	require.Contains(t, response.Content, "continue in the background")
	select {
	case <-exited:
	default:
		t.Fatal("wait worker was not joined")
	}
}
