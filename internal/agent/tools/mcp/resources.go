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
	snapshot := r.CatalogSnapshot()
	return func(yield func(string, []*Resource) bool) {
		for name, resources := range snapshot.Resources {
			if !yield(name, resources) {
				return
			}
		}
	}
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
	owner, published, ok := r.sessionOwner(name)
	if !ok || published != session {
		return nil, context.Canceled
	}
	resources, err := r.listResources(ctx, session)
	if err != nil {
		return nil, err
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !r.ownsSessionLocked(name, owner, session) {
		return resources, nil
	}
	r.catalogMu.Lock()
	if len(resources) == 0 {
		r.allResources.Del(name)
	} else {
		r.allResources.Set(name, resources)
	}
	r.catalogChanged()
	r.catalogMu.Unlock()
	prev, _ := r.states.Get(name)
	prev.Counts.Resources = len(resources)
	r.updateStateLocked(name, StateConnected, nil, session, prev.Counts)
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
	owner, session, ok := r.sessionOwner(name)
	if !ok {
		slog.Warn("Refresh resources: no session", "name", name)
		return
	}
	resources, err := r.listResources(ctx, session)
	if err != nil {
		r.updateStateForSession(name, owner, session, StateError, err, Counts{})
		return
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !r.ownsSessionLocked(name, owner, session) {
		return
	}
	r.catalogMu.Lock()
	if len(resources) == 0 {
		r.allResources.Del(name)
	} else {
		r.allResources.Set(name, resources)
	}
	r.catalogChanged()
	r.catalogMu.Unlock()
	prev, _ := r.states.Get(name)
	prev.Counts.Resources = len(resources)
	r.updateStateLocked(name, StateConnected, nil, session, prev.Counts)
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
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	if len(resources) == 0 {
		r.allResources.Del(name)
		r.catalogChanged()
		return 0
	}
	r.allResources.Set(name, resources)
	r.catalogChanged()
	return len(resources)
}
