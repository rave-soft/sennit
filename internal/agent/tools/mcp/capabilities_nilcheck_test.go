package mcp

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHasPromptsCapability is the regression test for a nil-pointer panic
// on getPrompts' capability check: a server that omits "capabilities"
// entirely from its InitializeResult (rather than sending an empty object)
// leaves Capabilities nil, and reading .Prompts off a nil *ServerCapabilities
// used to panic outright. Mirrors TestHasChannelCapability's style.
func TestHasPromptsCapability(t *testing.T) {
	t.Parallel()
	if hasPromptsCapability(nil) {
		t.Error("nil result should not have capability")
	}
	if hasPromptsCapability(&mcp.InitializeResult{}) {
		t.Error("nil capabilities should not have capability")
	}
	if hasPromptsCapability(&mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{}}) {
		t.Error("absent capability should be false")
	}
	res := &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{
		Prompts: &mcp.PromptCapabilities{},
	}}
	if !hasPromptsCapability(res) {
		t.Error("declared capability should be true")
	}
}

// TestHasResourcesCapability is the resources counterpart to
// TestHasPromptsCapability - see its doc comment.
func TestHasResourcesCapability(t *testing.T) {
	t.Parallel()
	if hasResourcesCapability(nil) {
		t.Error("nil result should not have capability")
	}
	if hasResourcesCapability(&mcp.InitializeResult{}) {
		t.Error("nil capabilities should not have capability")
	}
	if hasResourcesCapability(&mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{}}) {
		t.Error("absent capability should be false")
	}
	res := &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{
		Resources: &mcp.ResourceCapabilities{},
	}}
	if !hasResourcesCapability(res) {
		t.Error("declared capability should be true")
	}
}
