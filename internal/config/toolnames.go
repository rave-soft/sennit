package config

import (
	"slices"
	"time"

	"github.com/rave-soft/sennit/internal/brand"
)

// legacyToolNames maps names Braid used to expose onto the names its tools
// carry today. Tool names reach us from places we do not control and cannot
// migrate: a braid.json checked into a repo, a .braid/agents/*.md file, a
// `permissions allow ...` line in a shell config. Folding the old name onto
// the current one wherever such a list is read keeps those files working
// across a rename instead of silently ignoring an entry — an ignored
// allowed_tools entry means an unexpected permission prompt, and an ignored
// disabled_tools entry means a tool the user thought they had turned off.
//
// Kept as literals rather than referencing internal/agent/tools for its name
// constants: that package imports internal/config, so importing it back
// would cycle.
var legacyToolNames = map[string]string{
	// The read tool was called "view" until it took the name the UI had
	// always shown for it.
	"view": "read",
}

// CanonicalToolName folds a possibly-legacy tool name onto the name the
// agent registers today. Names it does not recognize pass through
// untouched: MCP tools (mcp_<server>_<tool>) and user-defined agents are
// named at runtime, so an unknown name here is not necessarily a wrong one.
func CanonicalToolName(name string) string {
	if current, ok := legacyToolNames[name]; ok {
		return current
	}
	return name
}

// canonicalToolNames applies [CanonicalToolName] across a list, dropping
// duplicates that the folding itself creates — a config listing both "view"
// and "read" must not end up with "read" twice.
func canonicalToolNames(names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = CanonicalToolName(name)
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

type Tools struct {
	Ls   ToolLs   `json:"ls,omitzero"`
	Grep ToolGrep `json:"grep,omitzero"`
	Glob ToolGlob `json:"glob,omitzero"`
}

type ToolLs struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for the ls tool,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for the ls tool,default=1000,example=100"`
}

// Limits returns the user-defined max-depth and max-items, or their defaults.
func (t ToolLs) Limits() (depth, items int) {
	return ptrValOr(t.MaxDepth, 0), ptrValOr(t.MaxItems, 0)
}

type ToolGrep struct {
	Timeout *time.Duration `json:"timeout,omitempty" jsonschema:"description=Timeout for the grep tool call,default=5s,example=10s"`
}

// GetTimeout returns the user-defined timeout or the default.
func (t ToolGrep) GetTimeout() time.Duration {
	return ptrValOr(t.Timeout, 5*time.Second)
}

type ToolGlob struct {
	Timeout *time.Duration `json:"timeout,omitempty" jsonschema:"description=Timeout for the glob tool call,default=30s,example=10s"`
}

// GetTimeout returns the user-defined timeout or the default.
func (t ToolGlob) GetTimeout() time.Duration {
	return ptrValOr(t.Timeout, 30*time.Second)
}

// AllToolNames returns the names of every built-in tool the agent can be
// given, in the order buildTools constructs them. It is the single source
// of truth for what a "known tool" is — used both to resolve
// disabled_tools/allowed_tools and (from internal/ui) to verify every
// tool has a rendering path.
func AllToolNames() []string {
	return allToolNames()
}

func allToolNames() []string {
	return []string{
		"agent",
		"bash",
		brand.ToolInfo,
		brand.ToolLogs,
		"job_output",
		"job_kill",
		"download",
		"edit",
		"multiedit",
		"lsp_diagnostics",
		"lsp_references",
		"lsp_restart",
		"lsp_symbols",
		"lsp_definition",
		"lsp_call_hierarchy",
		"lsp_rename",
		"lsp_replace_symbol",
		"fetch",
		"agentic_fetch",
		"web_fetch",
		"web_search",
		"glob",
		"grep",
		"ripgrep",
		"ls",
		"question",
		"todos",
		"read",
		"write",
		"list_mcp_resources",
		"read_mcp_resource",
		"thread_create",
		"thread_list",
		"thread_status",
		"thread_send",
		// thread_wait is deliberately absent from the default set: a
		// thread's completion now arrives on its own (the completion
		// inbox), so the blocking wait is no longer the model's only way
		// to learn a thread finished. The tool itself still exists for
		// the "wait for several threads before merging" case — see
		// tools.NewThreadWaitTool — and remains fully usable by any
		// agent config that explicitly lists it in AllowedTools; only the
		// default membership here changes.
		"thread_merge",
		"thread_remove",
		"task_list",
		"task_result",
		"task_cancel",
		"task_send",
		"task_output",
		"ask_parent",
	}
}

func resolveAllowedTools(allTools []string, disabledTools []string) []string {
	if disabledTools == nil {
		return allTools
	}
	// filter out disabled tools (exclude mode)
	return filterSlice(allTools, disabledTools, false)
}

func resolveReadOnlyTools(tools []string) []string {
	// fetch, web_fetch, and web_search don't modify local state, so they're
	// read-only in the same sense as glob/grep/view; the network calls they
	// make still go through the real permission.Service like the coder's.
	readOnlyTools := []string{"glob", "grep", "ripgrep", "ls", "lsp_call_hierarchy", "lsp_definition", "lsp_symbols", "read", "fetch", "web_fetch", "web_search"}
	// filter to only include tools that are in allowedtools (include mode)
	return filterSlice(tools, readOnlyTools, true)
}

func filterSlice(data []string, mask []string, include bool) []string {
	var filtered []string
	for _, s := range data {
		// if include is true, we include items that ARE in the mask
		// if include is false, we include items that are NOT in the mask
		if include == slices.Contains(mask, s) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}
