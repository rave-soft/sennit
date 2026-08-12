package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/rave-soft/braid/internal/proto"
)

// ListThreads lists all threads for a workspace.
func (c *Client) ListThreads(ctx context.Context, id string) ([]proto.Thread, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/threads", id), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list threads: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		if errors.Is(err, ErrConflict) {
			err = fmt.Errorf("%w: %w", ErrThreadsUnsupported, err)
		}
		return nil, fmt.Errorf("failed to list threads: %w", err)
	}
	var threads []proto.Thread
	if err := json.NewDecoder(rsp.Body).Decode(&threads); err != nil {
		return nil, fmt.Errorf("failed to decode threads: %w", err)
	}
	return threads, nil
}

// CreateThread creates a new thread in a workspace.
func (c *Client) CreateThread(ctx context.Context, id string, req proto.CreateThreadRequest) (*proto.Thread, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/threads", id), nil, jsonBody(req), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return nil, fmt.Errorf("failed to create thread: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to create thread: %w", err)
	}
	var st proto.Thread
	if err := json.NewDecoder(rsp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("failed to decode thread: %w", err)
	}
	return &st, nil
}

// GetThread retrieves a single thread from a workspace.
func (c *Client) GetThread(ctx context.Context, id, threadID string) (*proto.Thread, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/threads/%s", id, threadID), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get thread: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to get thread: %w", err)
	}
	var st proto.Thread
	if err := json.NewDecoder(rsp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("failed to decode thread: %w", err)
	}
	return &st, nil
}

// SendThread sends a follow-up message to a thread's session.
func (c *Client) SendThread(ctx context.Context, id, threadID, message string) error {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/threads/%s/send", id, threadID), nil,
		jsonBody(proto.SendThreadRequest{Message: message}),
		http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return fmt.Errorf("failed to send to thread: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return fmt.Errorf("failed to send to thread: %w", err)
	}
	return nil
}

// MergeThread merges (or retries merging) a thread.
func (c *Client) MergeThread(ctx context.Context, id, threadID string) (*proto.Thread, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/threads/%s/merge", id, threadID), nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to merge thread: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to merge thread: %w", err)
	}
	var st proto.Thread
	if err := json.NewDecoder(rsp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("failed to decode thread: %w", err)
	}
	return &st, nil
}

// RemoveThread removes a thread from a workspace.
func (c *Client) RemoveThread(ctx context.Context, id, threadID string, opts proto.RemoveThreadOptions) error {
	q := url.Values{}
	if opts.Force {
		q.Set("force", "true")
	}
	if opts.DeleteBranch {
		q.Set("delete_branch", "true")
	}
	rsp, err := c.delete(ctx, fmt.Sprintf("/workspaces/%s/threads/%s", id, threadID), q)
	if err != nil {
		return fmt.Errorf("failed to remove thread: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return fmt.Errorf("failed to remove thread: %w", err)
	}
	return nil
}
