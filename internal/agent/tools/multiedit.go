package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
)

type MultiEditOperation struct {
	OldString  string `json:"old_string" description:"The text to replace"`
	NewString  string `json:"new_string" description:"The text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace all occurrences of old_string (default false)."`
}

type MultiEditParams struct {
	FilePath string               `json:"file_path" description:"The absolute path to the file to modify"`
	Edits    []MultiEditOperation `json:"edits" description:"Array of edit operations to perform sequentially on the file"`
}

// MultiEditPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type MultiEditPermissionsParams = proto.MultiEditPermissionsParams

type FailedEdit struct {
	Index int                `json:"index"`
	Error string             `json:"error"`
	Edit  MultiEditOperation `json:"edit"`
}

type MultiEditResponseMetadata struct {
	Additions    int          `json:"additions"`
	Removals     int          `json:"removals"`
	OldContent   string       `json:"old_content,omitempty"`
	NewContent   string       `json:"new_content,omitempty"`
	EditsApplied int          `json:"edits_applied"`
	EditsFailed  []FailedEdit `json:"edits_failed,omitempty"`
}

const MultiEditToolName = "multiedit"

//go:embed multiedit.md
var multieditDescription string

func NewMultiEditTool(
	lspManager *lsp.Manager,
	permissions permission.Requester,
	files FileHistory,
	filetracker FileTracking,
	workingDir string,
) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		MultiEditToolName,
		multieditDescription,
		func(ctx context.Context, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return invalidParam("file_path"), nil
			}

			if len(params.Edits) == 0 {
				return fantasy.NewTextErrorResponse("at least one edit operation is required"), nil
			}

			params.FilePath = filepathext.SmartJoin(workingDir, params.FilePath)

			if msg, refused := confinementRefusal(permissions, params.FilePath); refused {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			// Validate all edits before applying any
			if err := validateEdits(params.Edits); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			var response fantasy.ToolResponse
			var err error

			editCtx := editContext{ctx, permissions, files, filetracker, workingDir}
			// Handle file creation case (first edit has empty old_string)
			if len(params.Edits) > 0 && params.Edits[0].OldString == "" {
				response, err = processMultiEditWithCreation(editCtx, params, call)
			} else {
				response, err = processMultiEditExistingFile(editCtx, params, call)
			}

			return finishMutation(ctx, lspManager, params.FilePath, response, err, func(content string) string {
				return fmt.Sprintf("<result>\n%s\n</result>\n", content)
			})
		},
	), map[string]toolParameterSchema{"file_path": {minLength: intPtr(1)}, "edits": {minItems: intPtr(1)}})
}

func validateEdits(edits []MultiEditOperation) error {
	for i, edit := range edits {
		// Only the first edit can have empty old_string (for file creation)
		if i > 0 && edit.OldString == "" {
			return fmt.Errorf("edit %d: only the first edit can have empty old_string (for file creation)", i+1)
		}
	}
	return nil
}

// formatFailedEditReasons renders one reason line per failed edit. The
// counts-only summary ("K edit(s) failed") that callers put in the
// success message doesn't say which edit failed or why; FailedEdit.Error
// otherwise only reached MultiEditResponseMetadata, which is rendered for
// a human but never appears in the text the model itself reads back.
func formatFailedEditReasons(failed []FailedEdit) string {
	var b strings.Builder
	b.WriteString("Failed edits:")
	for _, f := range failed {
		fmt.Fprintf(&b, "\n  edit %d: %s", f.Index, f.Error)
	}
	return b.String()
}

// applyEditsToContent applies edits sequentially, collecting the ones that
// failed. It also reports whether any edit only matched after whitespace
// normalization.
func applyEditsToContent(currentContent string, edits []MultiEditOperation, startIndex int) (string, []FailedEdit, bool) {
	var failedEdits []FailedEdit
	var whitespaceCorrected bool
	for i, edit := range edits {
		newContent, corrected, err := applyEditToContent(currentContent, edit)
		if err != nil {
			failedEdits = append(failedEdits, FailedEdit{
				Index: startIndex + i + 1,
				Error: err.Error(),
				Edit:  edit,
			})
			continue
		}
		whitespaceCorrected = whitespaceCorrected || corrected
		currentContent = newContent
	}
	return currentContent, failedEdits, whitespaceCorrected
}

func processMultiEditWithCreation(edit editContext, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	// First edit creates the file
	firstEdit := params.Edits[0]
	if firstEdit.OldString != "" {
		return fantasy.NewTextErrorResponse("first edit must have empty old_string for file creation"), nil
	}

	// Check if file already exists
	if _, err := os.Stat(params.FilePath); err == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", params.FilePath)), nil
	} else if !os.IsNotExist(err) {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	currentContent, failedEdits, whitespaceCorrected := applyEditsToContent(firstEdit.NewString, params.Edits[1:], 1)

	// Get session and message IDs
	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, missingSessionID("creating a new file")
	}

	editsApplied := len(params.Edits) - len(failedEdits)
	var description string
	if len(failedEdits) > 0 {
		description = fmt.Sprintf("Create file %s with %d of %d edits (%d failed)", params.FilePath, editsApplied, len(params.Edits), len(failedEdits))
	} else {
		description = fmt.Sprintf("Create file %s with %d edits", params.FilePath, editsApplied)
	}

	var message string
	if len(failedEdits) > 0 {
		message = fmt.Sprintf("File created with %d of %d edits: %s (%d edit(s) failed)\n%s",
			editsApplied, len(params.Edits), params.FilePath, len(failedEdits), formatFailedEditReasons(failedEdits))
	} else {
		message = fmt.Sprintf("File created with %d edits: %s", len(params.Edits), params.FilePath)
	}
	message = withWhitespaceNote(message, whitespaceCorrected)

	return applyFileMutation(fileMutationRequest{
		editContext: edit, call: call, filePath: params.FilePath, sessionID: sessionID, toolName: MultiEditToolName,
		prepare: func(snapshot fileSnapshot) (preparedFileMutation, error) {
			if snapshot.exists {
				return preparedFileMutation{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", params.FilePath)))
			}
			return preparedFileMutation{
				diffContent: currentContent, writeContent: currentContent, wholeFileRead: true,
				description: description, successMessage: message,
				permParams: MultiEditPermissionsParams{FilePath: params.FilePath, NewContent: currentContent},
				metadata: func(_, _ string, additions, removals int) any {
					return MultiEditResponseMetadata{NewContent: currentContent, Additions: additions, Removals: removals, EditsApplied: editsApplied, EditsFailed: failedEdits}
				},
			}, nil
		},
	})
}

func processMultiEditExistingFile(edit editContext, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	existing, err := loadExistingFile(edit, params.FilePath, "editing a file")
	if err != nil {
		var stop *mutationStop
		if errors.As(err, &stop) {
			return stop.Response, nil
		}
		return fantasy.ToolResponse{}, err
	}
	sessionID, oldContent := existing.sessionID, existing.oldContent

	currentContent, failedEdits, _ := applyEditsToContent(oldContent, params.Edits, 0)

	if resp, ok := requireReadCoverage(edit, sessionID, params.FilePath, oldContent, currentContent); !ok {
		return resp, nil
	}

	// Check if content actually changed
	if oldContent == currentContent {
		// If we have failed edits, report them
		if len(failedEdits) > 0 {
			return fantasy.WithResponseMetadata(
				fantasy.NewTextErrorResponse(fmt.Sprintf("no changes made - all %d edit(s) failed\n%s",
					len(failedEdits), formatFailedEditReasons(failedEdits))),
				MultiEditResponseMetadata{
					EditsApplied: 0,
					EditsFailed:  failedEdits,
				},
			), nil
		}
		return fantasy.NewTextErrorResponse("no changes made - all edits resulted in identical content"), nil
	}

	return applyFileMutation(fileMutationRequest{
		editContext: edit, call: call, filePath: params.FilePath, sessionID: sessionID, toolName: MultiEditToolName,
		prepare: func(snapshot fileSnapshot) (preparedFileMutation, error) {
			if !snapshot.exists || snapshot.isDir {
				return preparedFileMutation{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", params.FilePath)))
			}
			preparedContent, preparedFailures, preparedCorrected := applyEditsToContent(snapshot.content, params.Edits, 0)
			preparedApplied := len(params.Edits) - len(preparedFailures)
			var preparedDescription, preparedMessage string
			if len(preparedFailures) > 0 {
				preparedDescription = fmt.Sprintf("Apply %d of %d edits to file %s (%d failed)", preparedApplied, len(params.Edits), params.FilePath, len(preparedFailures))
				preparedMessage = fmt.Sprintf("Applied %d of %d edits to file: %s (%d edit(s) failed)\n%s",
					preparedApplied, len(params.Edits), params.FilePath, len(preparedFailures), formatFailedEditReasons(preparedFailures))
			} else {
				preparedDescription = fmt.Sprintf("Apply %d edits to file %s", preparedApplied, params.FilePath)
				preparedMessage = fmt.Sprintf("Applied %d edits to file: %s", preparedApplied, params.FilePath)
			}
			preparedWrite := preparedContent
			if snapshot.isCRLF {
				preparedWrite, _ = fsext.ToWindowsLineEndings(preparedContent)
			}
			return preparedFileMutation{
				diffContent: preparedContent, writeContent: preparedWrite,
				description: preparedDescription, successMessage: withWhitespaceNote(preparedMessage, preparedCorrected),
				permParams: MultiEditPermissionsParams{FilePath: params.FilePath, OldContent: snapshot.content, NewContent: preparedContent},
				metadata: func(_, _ string, additions, removals int) any {
					return MultiEditResponseMetadata{OldContent: snapshot.content, NewContent: preparedContent, Additions: additions, Removals: removals, EditsApplied: len(params.Edits) - len(preparedFailures), EditsFailed: preparedFailures}
				},
			}, nil
		},
	})
}

// applyEditToContent applies a single edit, reporting whether it only matched
// after whitespace normalization.
func applyEditToContent(content string, edit MultiEditOperation) (string, bool, error) {
	if edit.OldString == "" && edit.NewString == "" {
		return content, false, nil
	}

	if edit.OldString == "" {
		return "", false, fmt.Errorf("old_string cannot be empty for content replacement")
	}

	return findAndReplace(content, edit.OldString, edit.NewString, edit.ReplaceAll)
}
