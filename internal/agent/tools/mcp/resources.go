package mcp

import (
	"context"
	"errors"
	"iter"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/braid/internal/config"
)

type Resource = mcp.Resource

type ResourceContents = mcp.ResourceContents

// Resources returns all available MCP resources.
func Resources() iter.Seq2[string, []*Resource] { return defaultRegistry.Resources() }

func (r *Registry) Resources() iter.Seq2[string, []*Resource] {
	return r.allResources.Seq2()
}

// ListResources returns the current resources for an MCP server.
func ListResources(ctx context.Context, cfg *config.ConfigStore, name string) ([]*Resource, error) {
	return defaultRegistry.ListResources(ctx, cfg, name)
}

func (r *Registry) ListResources(ctx context.Context, cfg *config.ConfigStore, name string) ([]*Resource, error) {
	session, err := r.getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return nil, err
	}

	resources, err := getResources(ctx, session)
	if err != nil {
		return nil, err
	}

	resourceCount := r.updateResources(name, resources)
	prev, _ := r.states.Get(name)
	prev.Counts.Resources = resourceCount
	r.updateState(name, StateConnected, nil, session, prev.Counts)
	return resources, nil
}

// ReadResource reads the contents of a resource from an MCP server.
func ReadResource(ctx context.Context, cfg *config.ConfigStore, name, uri string) ([]*ResourceContents, error) {
	return defaultRegistry.ReadResource(ctx, cfg, name, uri)
}

func (r *Registry) ReadResource(ctx context.Context, cfg *config.ConfigStore, name, uri string) ([]*ResourceContents, error) {
	session, err := r.getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, err
	}
	return result.Contents, nil
}

// RefreshResources gets the updated list of resources from the MCP and updates the
// global state.
func RefreshResources(ctx context.Context, name string) { defaultRegistry.RefreshResources(ctx, name) }

func (r *Registry) RefreshResources(ctx context.Context, name string) {
	session, ok := r.sessions.Get(name)
	if !ok {
		slog.Warn("Refresh resources: no session", "name", name)
		return
	}

	resources, err := getResources(ctx, session)
	if err != nil {
		r.updateState(name, StateError, err, nil, Counts{})
		return
	}

	resourceCount := r.updateResources(name, resources)

	prev, _ := r.states.Get(name)
	prev.Counts.Resources = resourceCount
	r.updateState(name, StateConnected, nil, session, prev.Counts)
}

func getResources(ctx context.Context, c *ClientSession) ([]*Resource, error) {
	if c.InitializeResult().Capabilities.Resources == nil {
		return nil, nil
	}
	result, err := c.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		// Handle "Method not found" errors from MCP servers that don't support resources/list.
		if isMethodNotFoundError(err) {
			slog.Warn("MCP server does not support resources/list", "error", err)
			return nil, nil
		}
		return nil, err
	}
	return result.Resources, nil
}

// isMethodNotFoundError checks if the error is a JSON-RPC "Method not found" error.
func isMethodNotFoundError(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr != nil && rpcErr.Code == jsonrpc.CodeMethodNotFound
}

func (r *Registry) updateResources(name string, resources []*Resource) int {
	if len(resources) == 0 {
		r.allResources.Del(name)
		return 0
	}
	r.allResources.Set(name, resources)
	return len(resources)
}
