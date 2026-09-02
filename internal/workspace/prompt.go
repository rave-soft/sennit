package workspace

import "github.com/rave-soft/sennit/internal/skills"

// This file holds this package's own DTOs for the two command/prompt
// listings the Workspace interface exposes. They exist so workspace.go and
// read_only_workspace.go do not need to import internal/commands: that
// package also imports internal/agent/tools/mcp (for its LoadMCPPrompts /
// GetMCPPrompt helpers), which drags internal/hooks and
// internal/shellconfig along with it. Since internal/ui imports this
// package for the Workspace contract, that import chain reaches internal/ui
// too — see TestDomainPackageDoesNotDependOnAgentTransitively in
// dependency_guard_test.go, which fails today because workspace.go and
// read_only_workspace.go still spell their ListMCPPrompts/ListCustomCommands
// signatures as []commands.MCPPrompt / []commands.CustomCommand.
//
// internal/workspace/appws (github.com/rave-soft/sennit/internal/workspace/appws)
// is the boundary that should convert commands.MCPPrompt/CustomCommand/
// Argument into these types, the same way internal/app/threadspawn converts
// thread.Thread into proto.Thread. These DTOs are unused until that
// conversion lands and workspace.go/read_only_workspace.go switch their
// signatures over to them — see the dependency guard test for the
// remaining wiring this file does not do.

// Argument describes one argument a MCPPrompt or CustomCommand accepts.
// Mirrors commands.Argument.
type Argument struct {
	ID          string
	Title       string
	Description string
	Required    bool
}

// MCPPrompt describes a prompt loaded from an MCP server. Mirrors
// commands.MCPPrompt.
type MCPPrompt struct {
	ID          string
	Title       string
	Description string
	PromptID    string
	ClientID    string
	Arguments   []Argument
}

// CustomCommand describes a user-defined custom command loaded from
// markdown files, or a user-invocable skill surfaced as a command. Mirrors
// commands.CustomCommand. internal/skills is a leaf package (no
// internal/agent in its transitive closure — see dependency_guard_test.go),
// so its Skill type is safe to reference here directly rather than needing
// its own DTO.
type CustomCommand struct {
	ID        string
	Name      string
	Content   string
	Arguments []Argument
	// Skill is set when this command represents a user-invocable skill.
	Skill *skills.Skill
}
