package tools

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/diff"
	"github.com/rave-soft/braid/internal/filepathext"
	"github.com/rave-soft/braid/internal/filetracker"
	"github.com/rave-soft/braid/internal/fsext"
	"github.com/rave-soft/braid/internal/history"

	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/permission"
)

//go:embed write.md
var writeDescription string

type WriteParams struct {
	FilePath string `json:"file_path" description:"The path to the file to write"`
	Content  string `json:"content" description:"The content to write to the file"`
}

type WritePermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

type WriteResponseMetadata struct {
	Diff      string `json:"diff"`
	Additions int    `json:"additions"`
	Removals  int    `json:"removals"`
}

const WriteToolName = "write"

func NewWriteTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WriteToolName,
		writeDescription,
		func(ctx context.Context, params WriteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
			}

			filePath := filepathext.SmartJoin(workingDir, params.FilePath)

			if msg, refused := confinementRefusal(permissions, filePath); refused {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			fileInfo, err := os.Stat(filePath)
			if err == nil {
				if fileInfo.IsDir() {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Path is a directory, not a file: %s", filePath)), nil
				}

				modTime := fileInfo.ModTime().Truncate(time.Second)
				lastRead := filetracker.LastReadTime(ctx, sessionID, filePath)
				if modTime.After(lastRead) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"cannot write %s: it changed on disk after you last read it "+
							"(modified %s, last read %s).\n\n"+
							"Write replaces the whole file, so doing it now would discard "+
							"whatever was just written there by the user, a formatter, or "+
							"another agent.\n\n"+
							"Read %s to see the current content, then write the version that "+
							"keeps those changes.",
						filePath,
						modTime.Format(time.RFC3339), lastRead.Format(time.RFC3339),
						filePath,
					)), nil
				}

				oldContent, readErr := os.ReadFile(filePath)
				if readErr == nil && string(oldContent) == params.Content {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("File %s already contains the exact content. No changes made.", filePath)), nil
				}
			} else if !os.IsNotExist(err) {
				return fantasy.ToolResponse{}, fmt.Errorf("error checking file: %w", err)
			}

			if err := ensureParentDir(filePath); err != nil {
				return fantasy.ToolResponse{}, err
			}

			oldContent := ""
			if fileInfo != nil && !fileInfo.IsDir() {
				oldBytes, readErr := os.ReadFile(filePath)
				if readErr == nil {
					oldContent = string(oldBytes)
				}
			}

			diff, additions, removals := diff.GenerateDiff(
				oldContent,
				params.Content,
				strings.TrimPrefix(filePath, workingDir),
			)

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        fsext.PathOrPrefix(filePath, workingDir),
				ToolCallID:  call.ID,
				ToolName:    WriteToolName,
				Action:      "write",
				Description: fmt.Sprintf("Create file %s", filePath),
				Params: WritePermissionsParams{
					FilePath:   filePath,
					OldContent: oldContent,
					NewContent: params.Content,
				},
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return fantasy.WithResponseMetadata(resp, WriteResponseMetadata{
					Diff:      diff,
					Additions: additions,
					Removals:  removals,
				}), nil
			}

			if err := writeFileWithHistory(ctx, files, sessionID, filePath, oldContent, params.Content); err != nil {
				return fantasy.ToolResponse{}, err
			}
			recordWholeFileRead(ctx, filetracker, sessionID, filePath)

			notifyLSPs(ctx, lspManager, params.FilePath)

			result := fmt.Sprintf("File successfully written: %s", filePath)
			result = fmt.Sprintf("<result>\n%s\n</result>", result)
			result += getDiagnostics(filePath, lspManager)
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(result),
				WriteResponseMetadata{
					Diff:      diff,
					Additions: additions,
					Removals:  removals,
				},
			), nil
		},
	)
}
