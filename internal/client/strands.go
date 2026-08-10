package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/rave-soft/braid/internal/proto"
)

// ListStrands lists all strands for a workspace.
func (c *Client) ListStrands(ctx context.Context, id string) ([]proto.Strand, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/strands", id), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list strands: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to list strands: %w", err)
	}
	var strands []proto.Strand
	if err := json.NewDecoder(rsp.Body).Decode(&strands); err != nil {
		return nil, fmt.Errorf("failed to decode strands: %w", err)
	}
	return strands, nil
}

// CreateStrand creates a new strand in a workspace.
func (c *Client) CreateStrand(ctx context.Context, id string, req proto.CreateStrandRequest) (*proto.Strand, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/strands", id), nil, jsonBody(req), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return nil, fmt.Errorf("failed to create strand: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to create strand: %w", err)
	}
	var st proto.Strand
	if err := json.NewDecoder(rsp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("failed to decode strand: %w", err)
	}
	return &st, nil
}

// GetStrand retrieves a single strand from a workspace.
func (c *Client) GetStrand(ctx context.Context, id, strandID string) (*proto.Strand, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/strands/%s", id, strandID), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get strand: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to get strand: %w", err)
	}
	var st proto.Strand
	if err := json.NewDecoder(rsp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("failed to decode strand: %w", err)
	}
	return &st, nil
}

// SendStrand sends a follow-up message to a strand's session.
func (c *Client) SendStrand(ctx context.Context, id, strandID, message string) error {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/strands/%s/send", id, strandID), nil,
		jsonBody(proto.SendStrandRequest{Message: message}),
		http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return fmt.Errorf("failed to send to strand: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return fmt.Errorf("failed to send to strand: %w", err)
	}
	return nil
}

// MergeStrand merges (or retries merging) a strand.
func (c *Client) MergeStrand(ctx context.Context, id, strandID string) (*proto.Strand, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/strands/%s/merge", id, strandID), nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to merge strand: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to merge strand: %w", err)
	}
	var st proto.Strand
	if err := json.NewDecoder(rsp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("failed to decode strand: %w", err)
	}
	return &st, nil
}

// RemoveStrand removes a strand from a workspace.
func (c *Client) RemoveStrand(ctx context.Context, id, strandID string, opts proto.RemoveStrandOptions) error {
	q := url.Values{}
	if opts.Force {
		q.Set("force", "true")
	}
	if opts.DeleteBranch {
		q.Set("delete_branch", "true")
	}
	rsp, err := c.delete(ctx, fmt.Sprintf("/workspaces/%s/strands/%s", id, strandID), q)
	if err != nil {
		return fmt.Errorf("failed to remove strand: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return fmt.Errorf("failed to remove strand: %w", err)
	}
	return nil
}
