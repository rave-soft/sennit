package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/history"

	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
)

type EditParams struct {
	FilePath   string `json:"file_path" description:"The absolute path to the file to modify"`
	OldString  string `json:"old_string" description:"The text to replace"`
	NewString  string `json:"new_string" description:"The text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace all occurrences of old_string (default false)"`
}

type EditPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

type EditResponseMetadata struct {
	Additions  int    `json:"additions"`
	Removals   int    `json:"removals"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

const EditToolName = "edit"

//go:embed edit.md
var editDescription string

type editContext struct {
	ctx         context.Context
	permissions permission.Service
	files       history.Service
	filetracker filetracker.Service
	workingDir  string
}

func NewEditTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		EditToolName,
		editDescription,
		func(ctx context.Context, params EditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return invalidParam("file_path"), nil
			}

			params.FilePath = filepathext.SmartJoin(workingDir, params.FilePath)

			if msg, refused := confinementRefusal(permissions, params.FilePath); refused {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			var response fantasy.ToolResponse
			var err error

			editCtx := editContext{ctx, permissions, files, filetracker, workingDir}

			if params.OldString == "" {
				response, err = createNewFile(editCtx, params.FilePath, params.NewString, call)
			} else if params.NewString == "" {
				response, err = deleteContent(editCtx, params.FilePath, params.OldString, params.ReplaceAll, call)
			} else {
				response, err = replaceContent(editCtx, params.FilePath, params.OldString, params.NewString, params.ReplaceAll, call)
			}

			if err != nil {
				return response, err
			}
			if response.IsError {
				// Return early if there was an error during content replacement
				// This prevents unnecessary LSP diagnostics processing
				return response, nil
			}

			notifyLSPs(ctx, lspManager, params.FilePath)

			text := fmt.Sprintf("<result>\n%s\n</result>\n", response.Content)
			text += getDiagnostics(params.FilePath, lspManager)
			response.Content = text
			return response, nil
		},
	)
}

func createNewFile(edit editContext, filePath, content string, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	fileInfo, err := os.Stat(filePath)
	if err == nil {
		if fileInfo.IsDir() {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", filePath)), nil
	} else if !os.IsNotExist(err) {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	if err := ensureParentDir(filePath); err != nil {
		return fantasy.ToolResponse{}, err
	}

	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, missingSessionID("creating a new file")
	}

	return applyFileMutation(fileMutationRequest{
		editContext:    edit,
		call:           call,
		filePath:       filePath,
		sessionID:      sessionID,
		oldContent:     "",
		diffContent:    content,
		writeContent:   content,
		wholeFileRead:  true,
		toolName:       EditToolName,
		description:    fmt.Sprintf("Create file %s", filePath),
		successMessage: "File created: " + filePath,
		permParams: EditPermissionsParams{
			FilePath:   filePath,
			OldContent: "",
			NewContent: content,
		},
		metadata: func(content, _ string, additions, removals int) any {
			return EditResponseMetadata{OldContent: "", NewContent: content, Additions: additions, Removals: removals}
		},
	})
}

// findAndReplace performs a find-and-replace on content. When replaceAll is
// false it requires exactly one match. If an exact match fails, it falls back
// to whitespace-normalized matching and, failing that, returns a diagnostic
// hint describing why the replacement could not be made. The returned boolean
// reports whether the replacement relied on the whitespace-normalized
// fallback rather than an exact match.
func findAndReplace(content, old, new string, replaceAll bool) (string, bool, error) {
	if replaceAll {
		if strings.Contains(content, old) {
			return strings.ReplaceAll(content, old, new), false, nil
		}
	} else {
		index := strings.Index(content, old)
		switch {
		case index == -1:
			// Fall through to the fuzzy fallback below.
		case index != strings.LastIndex(content, old):
			return "", false, fmt.Errorf("old_string appears multiple times in the file. Please provide more context to ensure a unique match, or set replace_all to true")
		default:
			return content[:index] + new + content[index+len(old):], false, nil
		}
	}

	if result, ok := normalizedReplace(content, old, new, replaceAll); ok {
		return result, true, nil
	}
	return "", false, notFoundError(content, old)
}

// changedLineSpan reports the 1-based, inclusive span of oldContent's
// lines that differ from newContent, by trimming the common prefix and
// suffix. ok is false when the two are identical.
//
// It works off the before/after content rather than off old_string, so it
// covers every path that produces an edit — a literal replacement, a
// replace-all, the whitespace-normalized fuzzy fallback, and a multi-edit
// batch — instead of only the ones where old_string appears verbatim.
//
// A pure insertion (nothing in oldContent removed) reports the line the
// text is inserted after, clamped into the file: an insertion still needs
// its neighborhood to have been read to be placed correctly.
func changedLineSpan(oldContent, newContent string) (start, end int, ok bool) {
	if oldContent == newContent {
		return 0, 0, false
	}
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	start = prefix + 1
	end = len(oldLines) - suffix
	if end < start {
		// Pure insertion: no old line was consumed. Anchor on the line it
		// lands against so the check still asks for context.
		end = start
	}
	if start > len(oldLines) {
		start = len(oldLines)
		end = start
	}
	return start, end, true
}

// requireReadCoverage refuses an edit that touches lines this session
// never read. loadExistingFile already established that the file was read
// at all; this is the finer-grained half of the same rule, since the read
// tool serves windows — "I read this file" can mean fifty of its two
// thousand lines.
func requireReadCoverage(edit editContext, sessionID, filePath, oldContent, newContent string) (fantasy.ToolResponse, bool) {
	start, end, ok := changedLineSpan(oldContent, newContent)
	if !ok {
		return fantasy.ToolResponse{}, true
	}
	coverage := edit.filetracker.ReadCoverage(edit.ctx, sessionID, filePath)
	if coverage.Covers(start, end) {
		return fantasy.ToolResponse{}, true
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf(
		"cannot edit %s at lines %d-%d: that part of the file has not been read in this session.\n\n"+
			"Reads serve a window of the file, and only the lines in it were seen — %s. "+
			"Editing outside that window means old_string was recalled rather than copied, "+
			"which is how an edit silently lands on the wrong occurrence.\n\n"+
			"Read %s around line %d (use the offset parameter), then retry this edit.",
		filePath, start, end,
		describeCoverage(coverage),
		filePath, start,
	)), false
}

// describeCoverage renders coverage as a phrase for the error above.
func describeCoverage(c filetracker.Coverage) string {
	if len(c.Ranges) == 0 {
		return "no line range is on record"
	}
	parts := make([]string, 0, len(c.Ranges))
	for _, r := range c.Ranges {
		parts = append(parts, fmt.Sprintf("%d-%d", r.Start, r.End))
	}
	if len(parts) == 1 {
		return "so far that is line range " + parts[0]
	}
	return "so far those are line ranges " + strings.Join(parts, ", ")
}

// withWhitespaceNote appends the whitespace auto-correction note to a tool
// response message when the edit did not match the file byte-for-byte.
func withWhitespaceNote(message string, whitespaceCorrected bool) string {
	if !whitespaceCorrected {
		return message
	}
	return message + "\n" + whitespaceCorrectedNote
}

// notFoundError builds the "old_string not found" error, appending a
// diagnostic hint when one is available to help the caller self-correct.
func notFoundError(content, old string) error {
	msg := "old_string not found in file. Make sure it matches exactly, including whitespace and line breaks"
	if hint := diagnoseMismatch(content, old); hint != "" {
		msg += "\n\n" + hint
	}
	return errors.New(msg)
}

// existingFileResult is what loadExistingFile resolves for an edit that
// targets a file already on disk.
type existingFileResult struct {
	sessionID  string
	oldContent string
	isCrlf     bool
}

// loadExistingFile reads filePath's current on-disk state for an edit that
// targets an existing file. sessionAction names the action for
// missingSessionID's message (e.g. "editing a file") if no session ID is
// bound to the context.
func loadExistingFile(edit editContext, filePath, sessionAction string) (existingFileResult, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return existingFileResult{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", filePath)))
		}
		return existingFileResult{}, fmt.Errorf("failed to access file: %w", err)
	}

	if fileInfo.IsDir() {
		return existingFileResult{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)))
	}

	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return existingFileResult{}, missingSessionID(sessionAction)
	}

	switch state, lastRead := checkFileFreshness(edit.ctx, edit.filetracker, sessionID, filePath, fileInfo.ModTime()); state {
	case fileNeverRead:
		return existingFileResult{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf(
			"cannot edit %s: it has not been read in this session.\n\n"+
				"Edit replaces old_string with new_string literally, so old_string has to be "+
				"copied from the file as it is on disk right now — not recalled or guessed. "+
				"Reading it also records a baseline, which is how a later edit can tell that "+
				"the file changed underneath it.\n\n"+
				"Read %s, then retry this edit.",
			filePath, filePath,
		)))
	case fileStale:
		return existingFileResult{}, stopWith(fantasy.NewTextErrorResponse(fmt.Sprintf(
			"cannot edit %s: it changed on disk after you read it "+
				"(modified %s, last read %s).\n\n"+
				"Something outside this edit — the user, a formatter, a build step, another "+
				"agent — has written to the file, so old_string may no longer match what is "+
				"there, and editing now would overwrite that change.\n\n"+
				"Read %s again to see the current content, then redo the edit against it.",
			filePath,
			fileInfo.ModTime().Truncate(time.Second).Format(time.RFC3339), lastRead.Format(time.RFC3339),
			filePath,
		)))
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return existingFileResult{}, fmt.Errorf("failed to read file: %w", err)
	}

	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))
	return existingFileResult{sessionID: sessionID, oldContent: oldContent, isCrlf: isCrlf}, nil
}

// stringMutation is what deleteContent and replaceContent differ by. The
// fields are named rather than passed positionally because two of them are
// booleans: this is the write path, and a transposed pair would silently
// turn a rejected no-op edit into an accepted one, or a whole-file replace
// into a first-match one.
type stringMutation struct {
	filePath string
	// readAction names this operation in the "read the file first" error
	// loadExistingFile raises.
	readAction string
	oldString  string
	newString  string
	replaceAll bool
	// rejectNoOp refuses an edit that would leave the file unchanged.
	// Only replaceContent does: for a delete, "the text is not there" is
	// already reported by findAndReplace.
	rejectNoOp     bool
	description    string
	successMessage string
}

// applyStringMutation holds the body shared by deleteContent and
// replaceContent: load the file, run findAndReplace, enforce the
// read-before-edit coverage check, and hand the result to applyFileMutation.
// The two differed only in the fields of [stringMutation], so the plumbing
// around them is written once.
func applyStringMutation(edit editContext, m stringMutation, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	filePath := m.filePath
	existing, err := loadExistingFile(edit, filePath, m.readAction)
	if err != nil {
		var stop *mutationStop
		if errors.As(err, &stop) {
			return stop.Response, nil
		}
		return fantasy.ToolResponse{}, err
	}
	sessionID, oldContent, isCrlf := existing.sessionID, existing.oldContent, existing.isCrlf

	newContent, whitespaceCorrected, err := findAndReplace(oldContent, m.oldString, m.newString, m.replaceAll)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if m.rejectNoOp && newContent == oldContent {
		return fantasy.NewTextErrorResponse("new content is the same as old content. No changes made."), nil
	}
	if resp, ok := requireReadCoverage(edit, sessionID, filePath, oldContent, newContent); !ok {
		return resp, nil
	}

	writeContent := newContent
	if isCrlf {
		writeContent, _ = fsext.ToWindowsLineEndings(writeContent)
	}

	return applyFileMutation(fileMutationRequest{
		editContext:    edit,
		call:           call,
		filePath:       filePath,
		sessionID:      sessionID,
		oldContent:     oldContent,
		diffContent:    newContent,
		writeContent:   writeContent,
		toolName:       EditToolName,
		description:    m.description,
		successMessage: withWhitespaceNote(m.successMessage, whitespaceCorrected),
		permParams: EditPermissionsParams{
			FilePath:   filePath,
			OldContent: oldContent,
			NewContent: newContent,
		},
		metadata: func(content, _ string, additions, removals int) any {
			return EditResponseMetadata{OldContent: oldContent, NewContent: content, Additions: additions, Removals: removals}
		},
	})
}

func deleteContent(edit editContext, filePath, oldString string, replaceAll bool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return applyStringMutation(edit, stringMutation{
		filePath:       filePath,
		readAction:     "deleting content",
		oldString:      oldString,
		replaceAll:     replaceAll,
		description:    fmt.Sprintf("Delete content from file %s", filePath),
		successMessage: "Content deleted from file: " + filePath,
	}, call)
}

func replaceContent(edit editContext, filePath, oldString, newString string, replaceAll bool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return applyStringMutation(edit, stringMutation{
		filePath:       filePath,
		readAction:     "editing a file",
		oldString:      oldString,
		newString:      newString,
		replaceAll:     replaceAll,
		rejectNoOp:     true,
		description:    fmt.Sprintf("Replace content in file %s", filePath),
		successMessage: "Content replaced in file: " + filePath,
	}, call)
}
