package chat

import (
	"github.com/rave-soft/sennit/internal/home"
	tools "github.com/rave-soft/sennit/internal/proto"
)

// registerLSPToolRenderers registers the LSP tool renderers. Every one of
// them renders through [simpleToolRenderer] (see tools_simple.go): a
// pending spinner, a header built from a couple of params, the shared
// early-state block, then a plain line-count summary once a result lands.
// replace_symbol.go is the one LSP tool left out — it uses the full message
// width rather than the capped one, summarizes a diff instead of a line
// count, and shows an inline error tail on failure, so folding it into this
// table would mean special-casing most of the table's assumptions away.
func registerLSPToolRenderers() {
	registerToolRenderer(tools.DefinitionToolName, &definitionToolRenderer)
	registerToolRenderer(tools.ReferencesToolName, &referencesToolRenderer)
	registerToolRenderer(tools.RenameToolName, &renameToolRenderer)
	registerToolRenderer(tools.ReplaceSymbolToolName, &ReplaceSymbolToolRenderContext{})
	registerToolRenderer(tools.CallHierarchyToolName, &callHierarchyToolRenderer)
	registerToolRenderer(tools.SymbolsToolName, &symbolsToolRenderer)
	registerToolRenderer(tools.WorkspaceSymbolsToolName, &workspaceSymbolsToolRenderer)
	registerToolRenderer(tools.HoverToolName, &hoverToolRenderer)
	registerToolRenderer(tools.LSPRestartToolName, &lspRestartToolRenderer)
	registerToolRenderer(tools.DiagnosticsToolName, &diagnosticsToolRenderer)
}

var definitionToolRenderer = simpleToolRenderer{
	title: "Find Definition",
	params: func(opts *ToolRenderOpts) []string {
		p := decodeParams[tools.DefinitionParams](opts.ToolCall.Input)
		return []string{p.Symbol}
	},
	// Prefer the syntax-highlighted code metadata's line count; fall back
	// to the raw result content when there is no metadata (or it decoded
	// to nothing) to summarize instead.
	summary: func(opts *ToolRenderOpts) string {
		meta := decodeParams[tools.DefinitionResponseMetadata](opts.Result.Metadata)
		if meta.Content != "" {
			return lineCountSummary(meta.Content)
		}
		return lineCountSummary(opts.Result.Content)
	},
}

var referencesToolRenderer = simpleToolRenderer{
	title: "Find References",
	params: func(opts *ToolRenderOpts) []string {
		p := decodeParams[tools.ReferencesParams](opts.ToolCall.Input)
		params := []string{p.Symbol}
		if p.Path != "" {
			params = append(params, "path", home.ShortWithHome(opts.ServerHome, p.Path))
		}
		return params
	},
	summary: contentLineCountSummary,
}

var symbolsToolRenderer = simpleToolRenderer{
	title: "List Symbols",
	params: func(opts *ToolRenderOpts) []string {
		p := decodeParams[tools.SymbolsParams](opts.ToolCall.Input)
		return []string{p.FilePath}
	},
	summary: contentLineCountSummary,
}

var renameToolRenderer = simpleToolRenderer{
	title: "Rename Symbol",
	params: func(opts *ToolRenderOpts) []string {
		p := decodeParams[tools.RenameParams](opts.ToolCall.Input)
		params := []string{p.Symbol + " → " + p.NewName}
		if p.Path != "" {
			params = append(params, "path", home.ShortWithHome(opts.ServerHome, p.Path))
		}
		return params
	},
	summary: contentLineCountSummary,
}

var callHierarchyToolRenderer = simpleToolRenderer{
	title: "Call Hierarchy",
	params: func(opts *ToolRenderOpts) []string {
		p := decodeParams[tools.CallHierarchyParams](opts.ToolCall.Input)
		direction := "incoming"
		if p.Direction == "outgoing" {
			direction = "outgoing"
		}
		return []string{p.Symbol, direction}
	},
	summary: contentLineCountSummary,
}

var diagnosticsToolRenderer = simpleToolRenderer{
	title: "Diagnostics",
	params: func(opts *ToolRenderOpts) []string {
		p := decodeParams[tools.DiagnosticsParams](opts.ToolCall.Input)
		if p.FilePath == "" {
			return []string{"project"}
		}
		return []string{home.ShortWithHome(opts.ServerHome, p.FilePath)}
	},
	summary: contentLineCountSummary,
}

var lspRestartToolRenderer = simpleToolRenderer{
	title: "Restart LSP",
	params: func(opts *ToolRenderOpts) []string {
		p := decodeParams[tools.LSPRestartParams](opts.ToolCall.Input)
		if p.Name == "" {
			return nil
		}
		return []string{p.Name}
	},
	summary: contentLineCountSummary,
}

var (
	workspaceSymbolsToolRenderer = simpleToolRenderer{title: "Workspace Symbols", summary: contentLineCountSummary}
	hoverToolRenderer            = simpleToolRenderer{title: "Hover", summary: contentLineCountSummary}
)
