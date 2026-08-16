package chat

import (
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// toolItemFactory builds the concrete [ToolMessageItem] for a registered
// tool call. Most tools render through the plain [baseToolMessageItem]
// (returned by newBaseToolMessageItem); the few with extra item state —
// a click-to-expand body (bash/write/edit/multi-edit) or a nested-tool
// container (agentic_fetch) — wrap the base item in their own type.
type toolItemFactory func(sty *styles.Styles, toolCall message.ToolCall, result *message.ToolResult, canceled bool) ToolMessageItem

// toolRenderers is the registry that maps a tool name to the
// [ToolRenderer] that draws it. It replaces the former hand-rolled
// if/else chain over toolCall.Name: adding a renderer for a new tool is
// a new file plus a single line in registerToolRenderers (see
// registerToolRenderer below).
var toolRenderers = map[string]ToolRenderer{}

// registerToolRenderer records r as the renderer for toolName. A
// duplicate registration is a programming error — two renderers cannot
// own one tool name.
func registerToolRenderer(toolName string, r ToolRenderer) {
	if _, exists := toolRenderers[toolName]; exists {
		panic("chat: duplicate tool renderer registration for " + toolName)
	}
	toolRenderers[toolName] = r
}

// registerToolItemFactory pairs an item factory with the renderer it
// renders through, so NewToolMessageItem can consult a single registry
// for both.
func registerToolItemFactory(toolName string, factory toolItemFactory, r ToolRenderer) {
	registerToolRenderer(toolName, r)
	toolItemFactories[toolName] = factory
}

// toolItemFactories maps a tool name to the factory that builds its
// concrete [ToolMessageItem]. Populated alongside [toolRenderers] by
// registerToolItemFactory in the per-family files below.
var toolItemFactories = map[string]toolItemFactory{}

// registerToolRenderers wires every built-in tool's renderer into
// [toolRenderers] (and its item factory into [toolItemFactories] where
// one exists). Each family lives in its own file and registers itself
// here; a new tool needs only its file and one line below.
func registerToolRenderers() {
	registerBashToolRenderers()
	registerReadToolRenderer()
	registerEditToolRenderers()
	registerSearchToolRenderers()
	registerFetchToolRenderers()
	registerAgentToolRenderers()
	registerLSPToolRenderers()
	registerMiscToolRenderers()
}

func init() {
	registerToolRenderers()
}

// lookupToolRenderer returns the registered renderer for toolCall's name,
// or nil when the tool has no dedicated renderer and falls back to the
// generic one.
func lookupToolRenderer(name string) (ToolRenderer, toolItemFactory, bool) {
	r := toolRenderers[name]
	f := toolItemFactories[name]
	return r, f, r != nil
}
