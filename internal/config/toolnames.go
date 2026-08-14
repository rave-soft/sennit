package config

import "slices"

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
