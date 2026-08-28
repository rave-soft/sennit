package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
)

type ReplaceSymbolParams struct {
	Symbol      string `json:"symbol" description:"The symbol name to target (e.g., function name, method name, type name)"`
	FilePath    string `json:"file_path" description:"The path to the file containing the symbol"`
	Replacement string `json:"replacement,omitempty" description:"The replacement text. Required for 'replace' action. For 'add_before'/'add_after', the text to insert. Ignored for 'delete'."`
	Action      string `json:"action,omitempty" description:"Operation to perform: 'replace' (default, replace entire symbol), 'add_before' (insert before symbol), 'add_after' (insert after symbol), 'delete' (remove symbol entirely)"`
}

const ReplaceSymbolToolName = "lsp_replace_symbol"

//go:embed lsp_replace_symbol.md
var replaceSymbolDescription string

// ReplaceSymbolResponseMetadata carries diff data for the renderer.
type ReplaceSymbolResponseMetadata struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content"`
	NewContent string `json:"new_content"`
	Action     string `json:"action"`
}

// ReplaceSymbolPermissionsParams carries diff data for the permission dialog.
type ReplaceSymbolPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content"`
	NewContent string `json:"new_content"`
}

func NewReplaceSymbolTool(
	lspManager *lsp.Manager,
	permissions permission.Requester,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	tool := withToolParameterSchema(fantasy.NewAgentTool(
		ReplaceSymbolToolName,
		replaceSymbolDescription,
		func(ctx context.Context, params ReplaceSymbolParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Symbol == "" {
				return invalidParam("symbol"), nil
			}
			if params.FilePath == "" {
				return invalidParam("file_path"), nil
			}
			// See lsp_rename.go: a write tool with no session id refuses,
			// it does not write unasked. Checked before any LSP work, so
			// a call that cannot be permitted does no work at all.
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("replacing a symbol")
			}

			action := cmp.Or(params.Action, "replace")
			switch action {
			case "replace", "add_before", "add_after", "delete":
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid action %q: must be replace, add_before, add_after, or delete", action)), nil
			}
			if (action == "replace" || action == "add_before" || action == "add_after") && params.Replacement == "" {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("replacement is required for action %q", action)), nil
			}

			// Against the workspace, not the process cwd: a thread's
			// agent works in its own worktree while the process stays in
			// the main checkout, so a relative path here used to read and
			// write the wrong tree's file.
			filePath := filepathext.SmartJoin(workingDir, params.FilePath)

			lspManager.Start(ctx, filePath)

			client := findLSPClient(lspManager, filePath)
			if client == nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("no LSP client handles file: %s", params.FilePath)), nil
			}

			symbols, err := client.DocumentSymbols(ctx, filePath)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to get document symbols: %s", err)), nil
			}

			target := findSymbolByName(symbols, params.Symbol)
			if target == nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("symbol '%s' not found in %s", params.Symbol, params.FilePath)), nil
			}

			rng := target.GetRange()

			startLine := int(rng.Start.Line)
			endLine := int(rng.End.Line)
			resp, err := applyFileMutation(fileMutationRequest{
				editContext: editContext{ctx, permissions, files, filetracker, workingDir}, call: call,
				filePath: filePath, sessionID: sessionID, toolName: ReplaceSymbolToolName,
				prepare: func(snapshot fileSnapshot) (preparedFileMutation, error) {
					if !snapshot.exists {
						return preparedFileMutation{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", filePath)))
					}
					lines := strings.Split(snapshot.content, "\n")
					if startLine >= len(lines) || endLine >= len(lines) {
						return preparedFileMutation{}, stopWith(fantasy.NewTextErrorResponse("symbol range exceeds file length"))
					}
					var newLines []string
					switch action {
					case "replace":
						newLines = append(append(append([]string{}, lines[:startLine]...), strings.Split(params.Replacement, "\n")...), lines[endLine+1:]...)
					case "add_before":
						newLines = append(append(append([]string{}, lines[:startLine]...), strings.Split(params.Replacement, "\n")...), lines[startLine:]...)
					case "add_after":
						newLines = append(append(append([]string{}, lines[:endLine+1]...), strings.Split(params.Replacement, "\n")...), lines[endLine+1:]...)
					case "delete":
						newLines = append(append([]string{}, lines[:startLine]...), lines[endLine+1:]...)
					}
					newContent := strings.Join(newLines, "\n")
					writeContent := newContent
					if snapshot.isCRLF {
						writeContent, _ = fsext.ToWindowsLineEndings(newContent)
					}
					return preparedFileMutation{
						diffContent: newContent, writeContent: writeContent, wholeFileRead: true,
						description:    fmt.Sprintf("%s symbol '%s' in %s", action, params.Symbol, params.FilePath),
						permParams:     ReplaceSymbolPermissionsParams{FilePath: filePath, OldContent: snapshot.content, NewContent: newContent},
						successMessage: fmt.Sprintf("Updated symbol '%s' in %s", params.Symbol, params.FilePath),
						metadata: func(_, _ string, _, _ int) any {
							return ReplaceSymbolResponseMetadata{FilePath: filePath, OldContent: snapshot.content, NewContent: newContent, Action: action}
						},
					}, nil
				},
			})
			if err != nil && !mutationCommitted(err) || resp.IsError {
				return resp, err
			}
			notifyLSPs(ctx, lspManager, filePath)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}

			var summary string
			switch action {
			case "replace":
				summary = fmt.Sprintf("Replaced symbol '%s' in %s (lines %d-%d)", params.Symbol, params.FilePath, startLine+1, endLine+1)
			case "add_before":
				summary = fmt.Sprintf("Inserted before symbol '%s' in %s (before line %d)", params.Symbol, params.FilePath, startLine+1)
			case "add_after":
				summary = fmt.Sprintf("Inserted after symbol '%s' in %s (after line %d)", params.Symbol, params.FilePath, endLine+1)
			case "delete":
				summary = fmt.Sprintf("Deleted symbol '%s' from %s (lines %d-%d)", params.Symbol, params.FilePath, startLine+1, endLine+1)
			}

			resp.Content = summary + "\n" + getDiagnostics(filePath, lspManager)
			return resp, nil
		},
	), map[string]toolParameterSchema{"symbol": {minLength: intPtr(1)}, "file_path": {minLength: intPtr(1)}, "action": {enum: []string{"replace", "add_before", "add_after", "delete"}}})
	return withToolRootSchema(tool, map[string]any{"anyOf": []any{
		map[string]any{"required": []string{"action"}, "properties": map[string]any{"action": map[string]any{"const": "delete"}}},
		map[string]any{"required": []string{"action", "replacement"}, "properties": map[string]any{"action": map[string]any{"enum": []any{"replace", "add_before", "add_after"}}, "replacement": map[string]any{"type": "string", "minLength": 1}}},
		map[string]any{"required": []string{"replacement"}, "not": map[string]any{"required": []string{"action"}}, "properties": map[string]any{"replacement": map[string]any{"type": "string", "minLength": 1}}},
	}})
}

// findSymbolByName searches for a symbol by name in the document symbol tree.
func findSymbolByName(symbols []protocol.DocumentSymbolResult, name string) protocol.DocumentSymbolResult {
	for _, sym := range symbols {
		if sym.GetName() == name {
			return sym
		}
		if ds, ok := sym.(*protocol.DocumentSymbol); ok && len(ds.Children) > 0 {
			children := make([]protocol.DocumentSymbolResult, len(ds.Children))
			for i := range ds.Children {
				children[i] = &ds.Children[i]
			}
			if found := findSymbolByName(children, name); found != nil {
				return found
			}
		}
	}
	return nil
}
