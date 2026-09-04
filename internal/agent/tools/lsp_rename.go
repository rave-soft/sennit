package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"

	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/lsp"
	lsputil "github.com/rave-soft/sennit/internal/lsp/util"
	"github.com/rave-soft/sennit/internal/permission"
)

type RenameParams struct {
	Symbol  string `json:"symbol" description:"The symbol name to rename"`
	NewName string `json:"new_name" description:"The new name for the symbol"`
	Path    string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
}

const RenameToolName = "lsp_rename"

//go:embed lsp_rename.md
var renameDescription string

func NewRenameTool(
	lspManager *lsp.Manager,
	permissions permission.Requester,
	files FileHistory,
	filetracker FileTracking,
	workingDir string,
) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		RenameToolName,
		renameDescription,
		func(ctx context.Context, params RenameParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Symbol == "" {
				return invalidParam("symbol"), nil
			}
			if params.NewName == "" {
				return invalidParam("new_name"), nil
			}
			// A missing session id is an error, not a reason to skip the
			// prompt: this tool writes, and every other write tool refuses
			// rather than proceeding unasked (see missingSessionID's own
			// call sites). The old condition — sessionID != "" &&
			// permissions != nil — let a call with no session in context
			// rename across the whole workspace with no permission request
			// at all. Checked up front, before any LSP work.
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("renaming a symbol")
			}
			// Against the workspace, not the process cwd — see
			// NewDefinitionTool for why "." was the wrong tree in a
			// thread's worktree.
			searchDir := filepathext.SmartJoin(workingDir, params.Path)
			resolved, err := resolveSymbol(ctx, lspManager, params.Symbol, searchDir)
			if err != nil {
				if !isGenuineSymbolMiss(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("resolve symbol: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Symbol '%s' not found", params.Symbol)), nil
			}

			edit, err := resolved.client.Rename(ctx, resolved.path, resolved.line, resolved.char, params.NewName)
			if err != nil {
				slog.Error("Failed to rename symbol", "error", err, "symbol", params.Symbol)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("rename failed: %s", err)), nil
			}
			if edit == nil {
				return fantasy.NewTextResponse(fmt.Sprintf("No rename edits generated for symbol '%s'", params.Symbol)), nil
			}

			affectedFiles := collectAffectedFiles(edit)

			// The ranges above were computed against the server's overlay,
			// and nothing in this process sends didChange when a file
			// changes outside the edit tools — read and bash do not — so
			// for any file the client has open they can describe an older
			// version than the one ApplyWorkspaceEdit is about to read off
			// disk. Applying them then rewrites the wrong lines, which is
			// the defect lsp_replace_symbol closes for its own single
			// file; a rename spans however many files the symbol reaches,
			// and the set is only known once the server has answered.
			//
			// So: resync every open file this rename touches and ask
			// again. The second answer is the one computed against what is
			// actually on disk. Files the client never opened are already
			// read from disk by the server and need nothing. When nothing
			// was stale both answers agree and this costs one round trip.
			resynced, syncErr := resyncOpenFiles(ctx, resolved.client, append(affectedFiles, resolved.path))
			if syncErr != nil {
				// At least one open file failed to resync, so its overlay
				// is still stale and the edit computed above may target the
				// wrong lines. Refusing here is the whole point of this
				// block: silently falling through to "apply what we have"
				// is exactly the corruption it exists to prevent.
				return fantasy.NewTextErrorResponse(fmt.Sprintf("could not confirm the rename edits are current: %s", syncErr)), nil
			}
			if resynced {
				fresh, freshResolved, err := recomputeRename(ctx, lspManager, params, searchDir)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if fresh == nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Symbol '%s' moved while its rename was being computed; read the file and try again", params.Symbol)), nil
				}
				edit, resolved = fresh, freshResolved
				affectedFiles = collectAffectedFiles(edit)
			}

			// A rename is a write to every affected file, so the workspace
			// boundary applies to each of them, same as write/edit. Checked
			// before the permission request below, so a rename that will be
			// refused for writing outside the workspace does not first
			// interrupt the user with a prompt.
			for _, path := range affectedFiles {
				if msg, refused := confinementRefusal(permissions, path); refused {
					return fantasy.NewTextErrorResponse(msg), nil
				}
			}

			if permissions != nil {
				resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					ToolCallID:  call.ID,
					ToolName:    RenameToolName,
					Action:      "rename",
					Path:        searchDir,
					Params:      params,
					Description: fmt.Sprintf("Rename '%s' to '%s'", params.Symbol, params.NewName),
				})
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("permission request failed: %w", err)
				}
				if denied {
					return resp, nil
				}
			}

			// Captured once, up front, so both file history and the
			// read-coverage bookkeeping below diff against the same pre-edit
			// content instead of each reading the file for itself.
			preEditContent := make(map[string]string, len(affectedFiles))
			for _, path := range affectedFiles {
				content, err := os.ReadFile(path)
				if err != nil {
					slog.Warn("Failed to read file before rename", "path", path, "error", err)
					continue
				}
				preEditContent[path] = string(content)
			}

			if files != nil && sessionID != "" {
				for _, path := range affectedFiles {
					content, ok := preEditContent[path]
					if !ok {
						continue
					}
					if err := files.CreateVersion(ctx, sessionID, path, content); err != nil {
						slog.Warn("Failed to create file version", "path", path, "error", err)
					}
				}
			}

			encoding := resolved.client.GetOffsetEncoding()
			if err := lsputil.ApplyWorkspaceEdit(*edit, encoding); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to apply rename edits: %s", err)), nil
			}

			// Record only the lines the rename actually changed in each
			// affected file, not a full read of it — see recordEditedSpan's
			// doc comment in tools.go for why a whole-file read here would
			// hand back the blind-edit hole read-coverage exists to close.
			if filetracker != nil && sessionID != "" {
				for _, path := range affectedFiles {
					oldContent, ok := preEditContent[path]
					if !ok {
						continue
					}
					newRaw, err := os.ReadFile(path)
					if err != nil {
						slog.Warn("Failed to read file for read-coverage tracking", "path", path, "error", err)
						continue
					}
					normalizedOld, _ := fsext.ToUnixLineEndings(oldContent)
					normalizedNew, _ := fsext.ToUnixLineEndings(string(newRaw))
					recordEditedSpan(ctx, filetracker, sessionID, path, normalizedOld, normalizedNew)
				}
			}

			notifyLSPs(ctx, lspManager, "")

			var b strings.Builder
			fmt.Fprintf(&b, "Renamed '%s' to '%s' in %d file(s):\n\n", params.Symbol, params.NewName, len(affectedFiles))
			for _, f := range affectedFiles {
				fmt.Fprintf(&b, "  %s\n", f)
			}

			text := b.String()
			if len(affectedFiles) > 0 {
				text += "\n" + getDiagnostics(affectedFiles[0], lspManager)
			}

			return fantasy.NewTextResponse(text), nil
		},
	), map[string]toolParameterSchema{"symbol": {minLength: intPtr(1)}, "new_name": {minLength: intPtr(1)}})
}

// resyncOpenFiles re-reads from disk every file in paths that the client
// currently has open. It reports whether any file was open at all — the
// "nothing to do" case, which is not an error — separately from whether a
// resync it attempted actually succeeded. The caller must not treat these
// the same: "nothing was open" means the server's view was already current,
// while "a resync failed" means the server's view is still stale and the
// caller's edit may be wrong. A partial failure (some files resynced, one
// did not) is still reported as an error, since the caller cannot tell
// which of the affected files' overlays it can now trust.
func resyncOpenFiles(ctx context.Context, client *lsp.Client, paths []string) (resynced bool, err error) {
	seen := make(map[string]struct{}, len(paths))
	var failed []string
	for _, path := range paths {
		if _, done := seen[path]; done {
			continue
		}
		seen[path] = struct{}{}
		if !client.IsFileOpen(path) {
			continue
		}
		if syncErr := client.NotifyChange(ctx, path); syncErr != nil {
			slog.Debug("Failed to resync a file before recomputing a rename", "path", path, "error", syncErr)
			failed = append(failed, path)
			continue
		}
		resynced = true
	}
	if len(failed) > 0 {
		return resynced, fmt.Errorf("failed to resync %d file(s): %s", len(failed), strings.Join(failed, ", "))
	}
	return resynced, nil
}

// recomputeRename resolves the symbol and asks for its rename edits again,
// after resyncOpenFiles has brought the server's view back in line with
// disk. It returns a nil edit when the symbol no longer resolves, which
// means the file moved under the rename rather than that anything failed.
func recomputeRename(ctx context.Context, lspManager *lsp.Manager, params RenameParams, searchDir string) (*protocol.WorkspaceEdit, *resolvedSymbol, error) {
	resolved, err := resolveSymbol(ctx, lspManager, params.Symbol, searchDir)
	if err != nil {
		if isGenuineSymbolMiss(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("resolve symbol: %w", err)
	}
	edit, err := resolved.client.Rename(ctx, resolved.path, resolved.line, resolved.char, params.NewName)
	if err != nil {
		return nil, nil, fmt.Errorf("recompute rename: %w", err)
	}
	return edit, resolved, nil
}
