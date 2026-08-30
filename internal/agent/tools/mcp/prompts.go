package mcp

import (
	"context"
	"iter"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Prompt = mcp.Prompt

// Prompts returns all available MCP prompts.
func (r *Registry) Prompts() iter.Seq2[string, []*Prompt] {
	snapshot := r.CatalogSnapshot()
	return func(yield func(string, []*Prompt) bool) {
		for name, prompts := range snapshot.Prompts {
			if !yield(name, prompts) {
				return
			}
		}
	}
}

// GetPromptMessages retrieves the content of an MCP prompt with the given arguments.
func (r *Registry) GetPromptMessages(ctx context.Context, cfg ConfigProvider, clientName, promptName string, args map[string]string) ([]string, error) {
	c, err := r.getOrRenewClient(ctx, cfg, clientName)
	if err != nil {
		return nil, err
	}
	result, err := c.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      promptName,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}

	var messages []string
	for _, msg := range result.Messages {
		if msg.Role != "user" {
			continue
		}
		if textContent, ok := msg.Content.(*mcp.TextContent); ok {
			messages = append(messages, textContent.Text)
		}
	}
	return messages, nil
}

// RefreshPrompts gets the updated list of prompts from the MCP and updates the
// global state.
func (r *Registry) RefreshPrompts(ctx context.Context, name string) {
	owner, session, ok := r.sessionOwner(name)
	if !ok {
		slog.Warn("Refresh prompts: no session", "name", name)
		return
	}
	prompts, err := getPrompts(ctx, session)
	if err != nil {
		r.failStateForSession(name, owner, session, err)
		return
	}
	publishSingleCatalog(r, r.allPrompts, name, owner, session, prompts, func(c *Counts, n int) { c.Prompts = n })
}

// hasPromptsCapability reports whether a server's initialize result
// advertises prompt support. A server that omits "capabilities" entirely
// from InitializeResult (rather than sending an empty object) leaves
// Capabilities nil, matching hasChannelCapability's own nil check
// (channel.go) - guarding against it here avoids a nil-pointer panic
// reading .Prompts off it. On the startup path that panic would otherwise
// be recovered into a confusing StateError; on the renewal path
// (mcp-tools.go's tool-call goroutine) nothing in this package recovers it
// at all.
func hasPromptsCapability(res *mcp.InitializeResult) bool {
	return res != nil && res.Capabilities != nil && res.Capabilities.Prompts != nil
}

func getPrompts(ctx context.Context, c *ClientSession) ([]*Prompt, error) {
	if !hasPromptsCapability(c.InitializeResult()) {
		return nil, nil
	}
	result, err := c.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		return nil, err
	}
	return result.Prompts, nil
}
