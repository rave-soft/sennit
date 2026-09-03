package mcp

import (
	"context"
	"errors"
	"iter"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Resource = mcp.Resource

type ResourceContents = mcp.ResourceContents

// Resources returns all available MCP resources.
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
func (r *Registry) ListResources(ctx context.Context, cfg ConfigProvider, name string) ([]*Resource, error) {
	session, err := r.getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	owner, published, ok := r.sessionOwner(name)
	if !ok || published != session {
		return nil, errLostOwnership
	}
	resources, err := r.listResources(ctx, session)
	if err != nil {
		return nil, err
	}
	// Hand back the freshly fetched resources even if the commit below is
	// skipped because a concurrent teardown/reconnect has since taken over
	// the server — the caller asked for the current list, not for whether
	// this attempt still owns publication.
	publishSingleCatalog(r, r.allResources, name, owner, session, resources, func(c *Counts, n int) { c.Resources = n })
	return resources, nil
}

// ReadResource reads the contents of a resource from an MCP server.
func (r *Registry) ReadResource(ctx context.Context, cfg ConfigProvider, name, uri string) ([]*ResourceContents, error) {
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
func (r *Registry) RefreshResources(ctx context.Context, name string) {
	owner, session, ok := r.sessionOwner(name)
	if !ok {
		slog.Warn("Refresh resources: no session", "name", name)
		return
	}
	resources, err := r.listResources(ctx, session)
	if err != nil {
		r.failStateForSession(name, owner, session, err)
		return
	}
	publishSingleCatalog(r, r.allResources, name, owner, session, resources, func(c *Counts, n int) { c.Resources = n })
}

// hasResourcesCapability is the resources counterpart to
// hasPromptsCapability (prompts.go): see its doc comment.
func hasResourcesCapability(res *mcp.InitializeResult) bool {
	return res != nil && res.Capabilities != nil && res.Capabilities.Resources != nil
}

func getResources(ctx context.Context, c *ClientSession) ([]*Resource, error) {
	if !hasResourcesCapability(c.InitializeResult()) {
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
