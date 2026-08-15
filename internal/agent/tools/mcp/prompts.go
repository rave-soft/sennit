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
		r.updateStateForSession(name, owner, session, StateError, err, Counts{})
		return
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !r.ownsSessionLocked(name, owner, session) {
		return
	}
	r.catalogMu.Lock()
	if len(prompts) == 0 {
		r.allPrompts.Del(name)
	} else {
		r.allPrompts.Set(name, prompts)
	}
	r.catalogChanged()
	r.catalogMu.Unlock()
	prev, _ := r.states.Get(name)
	prev.Counts.Prompts = len(prompts)
	r.updateStateLocked(name, StateConnected, nil, session, prev.Counts)
}

func getPrompts(ctx context.Context, c *ClientSession) ([]*Prompt, error) {
	if c.InitializeResult().Capabilities.Prompts == nil {
		return nil, nil
	}
	result, err := c.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		return nil, err
	}
	return result.Prompts, nil
}
