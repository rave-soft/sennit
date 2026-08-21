package chat

import (
	tools "github.com/rave-soft/sennit/internal/proto"
)

// registerMiscToolRenderers registers the renderers of the tools that do
// not form a family of their own: todos and question. Diagnostics
// registers alongside the LSP family in lsp.go — it renders through the
// same [simpleToolRenderer] table.
func registerMiscToolRenderers() {
	registerToolRenderer(tools.TodosToolName, &TodosToolRenderContext{})
	registerToolRenderer(tools.QuestionToolName, &QuestionToolRenderContext{})
}
