package config

import (
	"slices"
	"time"

	"github.com/rave-soft/sennit/internal/toolmeta"
)

// CanonicalToolName folds a possibly-legacy tool name onto the name the
// agent registers today. Names it does not recognize pass through
// untouched: MCP tools (mcp_<server>_<tool>) and user-defined agents are
// named at runtime, so an unknown name here is not necessarily a wrong one.
func CanonicalToolName(name string) string {
	return toolmeta.CanonicalName(name)
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
	return toolmeta.DefaultNames()
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
	readOnlyTools := toolmeta.TaskReadOnlyNames()
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
