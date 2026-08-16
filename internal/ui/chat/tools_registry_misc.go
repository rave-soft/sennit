package chat

import (
	tools "github.com/rave-soft/sennit/internal/proto"
)

// registerMiscToolRenderers registers the renderers of the tools that do
// not form a family of their own: diagnostics, todos and question.
func registerMiscToolRenderers() {
	registerToolRenderer(tools.DiagnosticsToolName, &DiagnosticsToolRenderContext{})
	registerToolRenderer(tools.TodosToolName, &TodosToolRenderContext{})
	registerToolRenderer(tools.QuestionToolName, &QuestionToolRenderContext{})
}
