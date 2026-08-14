package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/rave-soft/braid/internal/proto"
)

// ListTasks lists all tasks for a workspace. Mirrors ListThreads.
func (c *Client) ListTasks(ctx context.Context, id string) ([]proto.Thread, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/tasks", id), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		if errors.Is(err, ErrConflict) {
			err = fmt.Errorf("%w: %w", ErrTasksUnsupported, err)
		}
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	var tasks []proto.Thread
	if err := json.NewDecoder(rsp.Body).Decode(&tasks); err != nil {
		return nil, fmt.Errorf("failed to decode tasks: %w", err)
	}
	return tasks, nil
}

// CancelTask cancels a task's in-flight run, recording reason as its
// terminal error, and returns the resulting state. Mirrors MergeThread's
// shape (a POST that returns the updated proto.Thread) — closer to that
// than to a bare-error thread action like SendThread, since a cancel's
// caller (the TUI panel) wants the settled status back immediately rather
// than waiting on the next list refresh.
func (c *Client) CancelTask(ctx context.Context, id, taskID, reason string) (*proto.Thread, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/tasks/%s/cancel", id, taskID), nil,
		jsonBody(proto.CancelDelegationRequest{Reason: reason}),
		http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return nil, fmt.Errorf("failed to cancel task: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to cancel task: %w", err)
	}
	var st proto.Thread
	if err := json.NewDecoder(rsp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("failed to decode task: %w", err)
	}
	return &st, nil
}
