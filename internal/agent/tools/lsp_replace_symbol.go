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
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
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

// ReplaceSymbolPermissionsParams carries diff data for the permission
// dialog. Defined in proto; see the comment on BashPermissionsParams in
// bash.go.
type ReplaceSymbolPermissionsParams = proto.ReplaceSymbolPermissionsParams

func NewReplaceSymbolTool(
	lspManager *lsp.Manager,
	permissions permission.Requester,
	files FileHistory,
	filetracker FileTracking,
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

			// Sync the overlay with disk before asking for symbol ranges.
			// OpenFileOnDemand is a no-op once the file is already open, and
			// neither read nor bash ever sends a didChange, so the LSP's
			// view of the file can be older than what's on disk (e.g. after
			// gofmt, an editor save, or another tool's write). NotifyChange
			// re-reads the file whole and bumps the LSP's version
			// (filesync.go), so DocumentSymbols below reports ranges
			// against the same content applyFileMutation is about to read
			// and cut. Two calls, not one: NotifyChange itself errors on a
			// file the LSP has never opened.
			if err := client.OpenFileOnDemand(ctx, filePath); err != nil {
				if ctx.Err() != nil {
					return fantasy.ToolResponse{}, ctx.Err()
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to open file for LSP: %s", err)), nil
			}
			if err := client.NotifyChange(ctx, filePath); err != nil {
				if ctx.Err() != nil {
					return fantasy.ToolResponse{}, ctx.Err()
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to sync file with LSP: %s", err)), nil
			}

			symbols, err := client.DocumentSymbols(ctx, filePath)
			if err != nil {
				// A canceled context (Esc) must abort the tool-call batch
				// like any other infrastructure failure, not read back to
				// the model as "failed to get document symbols: context
				// canceled" — see lsp_helpers.go's rule for the sibling
				// symbol-lookup tools.
				if ctx.Err() != nil {
					return fantasy.ToolResponse{}, ctx.Err()
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to get document symbols: %s", err)), nil
			}

			target := findSymbolByName(symbols, params.Symbol)
			if target == nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("symbol '%s' not found in %s", params.Symbol, params.FilePath)), nil
			}

			rng := target.GetRange()

			startLine := int(rng.Start.Line)
			endLine := symbolRangeEndLine(rng)
			resp, err := applyFileMutation(fileMutationRequest{
				editContext: editContext{ctx, permissions, files, filetracker, workingDir}, call: call,
				filePath: filePath, sessionID: sessionID, toolName: ReplaceSymbolToolName,
				prepare: func(snapshot fileSnapshot) (preparedFileMutation, error) {
					if !snapshot.exists {
						return preparedFileMutation{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", filePath)))
					}
					// Resyncing the overlay above (client.NotifyChange) only
					// fixes the LSP's own view of the file; it says nothing
					// about whether the session has ever seen this content.
					// Without this check, the model could replace a symbol
					// in a file it never read — or one a formatter rewrote
					// after its last read — same as edit/write/multiedit
					// already refuse for their own mutations.
					switch state, lastRead := checkFileFreshness(ctx, filetracker, sessionID, filePath, snapshot.modTime); state {
					case fileNeverRead:
						return preparedFileMutation{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf(
							"cannot replace symbol '%s' in %s: it has not been read in this session.\n\n"+
								"This tool cuts the file at the range the LSP reports for the symbol, so "+
								"reading it first is how a later mutation can tell whether the file changed "+
								"underneath it.\n\n"+
								"Read %s, then retry.",
							params.Symbol, filePath, filePath,
						)))
					case fileStale:
						return preparedFileMutation{}, stopWith(fantasy.NewTextErrorResponse(staleFileRefusal(
							filePath, "replace a symbol in", "after you read it",
							"Something outside this call — the user, a formatter, a build step, another "+
								"agent — has written to the file, so the symbol's line range may no longer "+
								"match what's there, and replacing it now could cut the wrong span.",
							fmt.Sprintf("Read %s again to see the current content, then redo the replacement.", filePath),
							snapshot.modTime, lastRead,
						)))
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
					// wholeFileRead stays false: the model supplied a
					// replacement for one symbol's range, not the file, so
					// only that span counts as seen. Marking the whole file
					// read here would let a later edit or write change any
					// line of a file the session never opened.
					return preparedFileMutation{
						diffContent: newContent, writeContent: writeContent, wholeFileRead: false,
						description:    fmt.Sprintf("%s symbol '%s' in %s", action, params.Symbol, params.FilePath),
						permParams:     ReplaceSymbolPermissionsParams{FilePath: filePath, OldContent: snapshot.content, NewContent: newContent},
						successMessage: fmt.Sprintf("Updated symbol '%s' in %s", params.Symbol, params.FilePath),
						metadata: func(_, _ string, _, _ int) any {
							return ReplaceSymbolResponseMetadata{FilePath: filePath, OldContent: snapshot.content, NewContent: newContent, Action: action}
						},
					}, nil
				},
			})
			return finishMutation(ctx, lspManager, filePath, resp, err, func(_ string) string {
				// Unlike edit/multiedit/write, the response body here isn't
				// framed from resp.Content — it's a summary computed from
				// the symbol range, so wrap ignores the content it is given.
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
				return summary + "\n"
			})
		},
	), map[string]toolParameterSchema{"symbol": {minLength: intPtr(1)}, "file_path": {minLength: intPtr(1)}, "action": {enum: []string{"replace", "add_before", "add_after", "delete"}}})
	return withToolRootSchema(tool, map[string]any{"anyOf": []any{
		map[string]any{"required": []string{"action"}, "properties": map[string]any{"action": map[string]any{"const": "delete"}}},
		map[string]any{"required": []string{"action", "replacement"}, "properties": map[string]any{"action": map[string]any{"enum": []any{"replace", "add_before", "add_after"}}, "replacement": map[string]any{"type": "string", "minLength": 1}}},
		map[string]any{"required": []string{"replacement"}, "not": map[string]any{"required": []string{"action"}}, "properties": map[string]any{"replacement": map[string]any{"type": "string", "minLength": 1}}},
	}})
}

// symbolRangeEndLine returns the 0-indexed last line rng actually covers.
//
// LSP ranges are end-exclusive: per protocol.Range's own doc comment, a
// range spanning through the end of line 5 (0-indexed) is reported with
// End:{Line:6, Character:0} - the start of the FOLLOWING line, not a
// position on the last line itself. Treating End.Line as an inclusive line
// index in that case eats one line too many on delete/replace, or inserts
// one line too late on add_after; back it off to the true last line
// whenever End lands at column 0 of a line after Start's.
func symbolRangeEndLine(rng protocol.Range) int {
	endLine := int(rng.End.Line)
	if rng.End.Character == 0 && rng.End.Line > rng.Start.Line {
		endLine--
	}
	return endLine
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
