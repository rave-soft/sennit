package tools

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/fsext"
	historystore "github.com/rave-soft/sennit/internal/history/store"

	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
)

//go:embed write.md
var writeDescription string

type WriteParams struct {
	FilePath string `json:"file_path" description:"The path to the file to write"`
	Content  string `json:"content" description:"The content to write to the file"`
}

// WritePermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type WritePermissionsParams = proto.WritePermissionsParams

type WriteResponseMetadata struct {
	Diff      string `json:"diff"`
	Additions int    `json:"additions"`
	Removals  int    `json:"removals"`
}

const WriteToolName = "write"

func NewWriteTool(
	lspManager *lsp.Manager,
	permissions permission.Requester,
	files historystore.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		WriteToolName,
		writeDescription,
		func(ctx context.Context, params WriteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return invalidParam("file_path"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("creating a new file")
			}

			filePath := filepathext.SmartJoin(workingDir, params.FilePath)

			if msg, refused := confinementRefusal(permissions, filePath); refused {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			var oldContent string
			fileInfo, err := os.Stat(filePath)
			switch {
			case err == nil:
				if fileInfo.IsDir() {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Path is a directory, not a file: %s", filePath)), nil
				}

				switch state, lastRead := checkFileFreshness(ctx, filetracker, sessionID, filePath, fileInfo.ModTime()); state {
				case fileNeverRead:
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"cannot write %s: it has not been read in this session.\n\n"+
							"Write replaces the whole file, so doing it now could discard "+
							"whatever is currently there without your knowledge — content "+
							"written by the user, a formatter, or another agent.\n\n"+
							"Read %s, then write the version that keeps those changes.",
						filePath, filePath,
					)), nil
				case fileStale:
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"cannot write %s: it changed on disk after you last read it "+
							"(modified %s, last read %s).\n\n"+
							"Write replaces the whole file, so doing it now would discard "+
							"whatever was just written there by the user, a formatter, or "+
							"another agent.\n\n"+
							"Read %s to see the current content, then write the version that "+
							"keeps those changes.",
						filePath,
						fileInfo.ModTime().Truncate(time.Second).Format(time.RFC3339), lastRead.Format(time.RFC3339),
						filePath,
					)), nil
				}

				oldBytes, readErr := os.ReadFile(filePath)
				if readErr != nil {
					// The file is there (the stat above succeeded) but
					// cannot be read. Carrying on with an empty
					// oldContent told the diff and the permission dialog
					// this was a brand-new file, and stored an empty
					// baseline in history — so the version the user
					// would restore to was blank.
					return fantasy.ToolResponse{}, fmt.Errorf("read existing file: %w", readErr)
				}
				oldContent = string(oldBytes)
				if oldContent == params.Content {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("File %s already contains the exact content. No changes made.", filePath)), nil
				}
			case !os.IsNotExist(err):
				return fantasy.ToolResponse{}, fmt.Errorf("error checking file: %w", err)
			}

			resp, err := applyFileMutation(fileMutationRequest{
				editContext: editContext{ctx, permissions, files, filetracker, workingDir},
				call:        call, filePath: filePath, sessionID: sessionID, toolName: WriteToolName,
				prepare: func(snapshot fileSnapshot) (preparedFileMutation, error) {
					if snapshot.isDir {
						return preparedFileMutation{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf("Path is a directory, not a file: %s", filePath)))
					}
					writeContent := params.Content
					if snapshot.isCRLF {
						writeContent, _ = fsext.ToWindowsLineEndings(params.Content)
					}
					return preparedFileMutation{
						diffContent: params.Content, writeContent: writeContent, wholeFileRead: true,
						description:    fmt.Sprintf("Create file %s", filePath),
						successMessage: fmt.Sprintf("File successfully written: %s", filePath),
						permParams:     WritePermissionsParams{FilePath: filePath, OldContent: snapshot.content, NewContent: params.Content},
						metadata: func(_, diffText string, additions, removals int) any {
							return WriteResponseMetadata{Diff: diffText, Additions: additions, Removals: removals}
						},
					}, nil
				},
			})
			if err != nil && !mutationCommitted(err) {
				return fantasy.ToolResponse{}, err
			}
			if resp.IsError {
				return resp, nil
			}

			// The resolved path, not the raw parameter: a relative one
			// never matched any client's cwd, so the LSP was told nothing
			// about a file this tool had just written and went on serving
			// diagnostics for the old content. getDiagnostics on the next
			// line already used the resolved form.
			notifyLSPs(ctx, lspManager, filePath)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			resp.Content = fmt.Sprintf("<result>\n%s\n</result>", resp.Content) + getDiagnostics(filePath, lspManager)
			return resp, nil
		},
	), map[string]toolParameterSchema{"file_path": {minLength: intPtr(1)}})
}
