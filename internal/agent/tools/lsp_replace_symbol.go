package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/filetracker"
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
	permissions permission.Service,
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

			content, err := os.ReadFile(filePath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
			}

			lines := strings.Split(string(content), "\n")
			startLine := int(rng.Start.Line)
			endLine := int(rng.End.Line)
			if startLine >= len(lines) || endLine >= len(lines) {
				return fantasy.NewTextErrorResponse("symbol range exceeds file length"), nil
			}

			// Compute new content before permission so the dialog can show a diff.
			var newLines []string
			switch action {
			case "replace":
				newLines = make([]string, 0, len(lines))
				newLines = append(newLines, lines[:startLine]...)
				newLines = append(newLines, strings.Split(params.Replacement, "\n")...)
				newLines = append(newLines, lines[endLine+1:]...)
			case "add_before":
				newLines = make([]string, 0, len(lines)+strings.Count(params.Replacement, "\n")+1)
				newLines = append(newLines, lines[:startLine]...)
				newLines = append(newLines, strings.Split(params.Replacement, "\n")...)
				newLines = append(newLines, lines[startLine:]...)
			case "add_after":
				newLines = make([]string, 0, len(lines)+strings.Count(params.Replacement, "\n")+1)
				newLines = append(newLines, lines[:endLine+1]...)
				newLines = append(newLines, strings.Split(params.Replacement, "\n")...)
				newLines = append(newLines, lines[endLine+1:]...)
			case "delete":
				newLines = make([]string, 0, len(lines))
				newLines = append(newLines, lines[:startLine]...)
				newLines = append(newLines, lines[endLine+1:]...)
			}

			newContent := strings.Join(newLines, "\n")

			if msg, refused := confinementRefusal(permissions, filePath); refused {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			if permissions != nil {
				resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					ToolCallID:  call.ID,
					Path:        filePath,
					ToolName:    ReplaceSymbolToolName,
					Description: fmt.Sprintf("%s symbol '%s' in %s", action, params.Symbol, params.FilePath),
					Params: ReplaceSymbolPermissionsParams{
						FilePath:   filePath,
						OldContent: string(content),
						NewContent: newContent,
					},
				})
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("permission request failed: %w", err)
				}
				if denied {
					return resp, nil
				}
			}

			if files != nil && sessionID != "" {
				if _, err := files.CreateVersion(ctx, sessionID, filePath, string(content)); err != nil {
					slog.Warn("Failed to create file version before replace", "path", filePath, "error", err)
				}
			}

			if err := os.WriteFile(filePath, []byte(newContent), 0o644); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
			}

			if filetracker != nil && sessionID != "" {
				filetracker.RecordRead(ctx, sessionID, filePath)
			}

			notifyLSPs(ctx, lspManager, filePath)

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

			resp := fantasy.NewTextResponse(summary + "\n" + getDiagnostics(filePath, lspManager))
			resp = fantasy.WithResponseMetadata(resp, ReplaceSymbolResponseMetadata{
				FilePath:   filePath,
				OldContent: string(content),
				NewContent: newContent,
				Action:     action,
			})
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
