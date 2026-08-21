package tools

import (
	"context"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/diff"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/permission"
)

// fileFreshness classifies a file's on-disk state against what this
// session has recorded reading of it.
type fileFreshness int

const (
	fileFresh     fileFreshness = iota // read, and unmodified since
	fileNeverRead                      // no read recorded for this session
	fileStale                          // modified on disk after the last read
)

// checkFileFreshness is the "did this file change out from under the
// model" comparison behind write/edit/multiedit's staleness refusals. It
// used to be implemented twice — inline in write.go, and in edit.go's
// loadExistingFile — and had drifted: only edit.go separated "never read"
// from "modified after read"; write.go compared mtime against a zero
// time.Time either way, so a never-read file got told it was "modified" at
// "0001-01-01T00:00:00Z". Both outcomes still refused, so unifying only
// fixes the message.
func checkFileFreshness(ctx context.Context, ft filetracker.Service, sessionID, filePath string, modTime time.Time) (state fileFreshness, lastRead time.Time) {
	lastRead = ft.LastReadTime(ctx, sessionID, filePath)
	switch {
	case lastRead.IsZero():
		return fileNeverRead, lastRead
	case modTime.Truncate(time.Second).After(lastRead):
		return fileStale, lastRead
	default:
		return fileFresh, lastRead
	}
}

// mutationStop is the error a precondition check returns to mean "stop and
// return this response", not a real failure — replacing the old
// loadExistingFile shape, which returned a sentinel zero-value
// fantasy.ToolResponse a caller could forget to check. Callers unwrap it
// with errors.As.
type mutationStop struct {
	Response fantasy.ToolResponse
}

func (e *mutationStop) Error() string { return "file mutation stopped: " + e.Response.Content }

func stopWith(resp fantasy.ToolResponse) error { return &mutationStop{Response: resp} }

// fileMutationRequest is the shared commit-phase input for write, edit, and
// multiedit once each has resolved its own preconditions (confinement,
// existence, staleness — these stay with each tool, since their messages
// and existence rules differ; see checkFileFreshness for the piece they
// share) and has old/new content in hand. It embeds editContext for the
// service dependencies every caller already has one of in scope.
type fileMutationRequest struct {
	editContext
	call fantasy.ToolCall

	filePath  string
	sessionID string

	oldContent string // content on disk before the mutation; "" when creating
	// diffContent is compared against oldContent for the diff and the
	// permission dialog; writeContent is what lands on disk. They differ
	// only for CRLF files, diffed in normalized LF form but written back
	// with CRLF restored.
	diffContent  string
	writeContent string
	// wholeFileRead marks the entire file as read after the write (Write,
	// and file creation through Edit/MultiEdit: the caller supplied the
	// whole file). Otherwise only the changed span is recorded.
	wholeFileRead bool

	toolName       string
	description    string
	permParams     any
	successMessage string // tool response text once the file is written

	// metadata builds the tool's own response-metadata type (Write/Edit/
	// MultiEdit each have a different shape, so the type stays with the
	// caller). It runs either way: on denial with content == diffContent
	// (nothing written), on success with content == writeContent (what
	// landed on disk) — the two differ only for CRLF files.
	metadata func(content, diffText string, additions, removals int) any
}

// applyFileMutation runs the sequence write/edit/multiedit all shared once
// content is ready to commit: diff, permission request, file write with
// history, filetracker recording, and — either way — the tool's response.
// A denial short-circuits before the write, metadata built against
// diffContent; success writes and records first, then builds it against
// writeContent.
func applyFileMutation(req fileMutationRequest) (fantasy.ToolResponse, error) {
	diffText, additions, removals := diff.GenerateDiff(
		req.oldContent,
		req.diffContent,
		strings.TrimPrefix(req.filePath, req.workingDir),
	)

	resp, denied, err := requirePermission(req.ctx, req.permissions, permission.CreatePermissionRequest{
		SessionID:   req.sessionID,
		Path:        fsext.PathOrPrefix(req.filePath, req.workingDir),
		ToolCallID:  req.call.ID,
		ToolName:    req.toolName,
		Action:      "write",
		Description: req.description,
		Params:      req.permParams,
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if denied {
		return fantasy.WithResponseMetadata(resp, req.metadata(req.diffContent, diffText, additions, removals)), nil
	}

	if err := writeFileWithHistory(req.ctx, req.files, req.sessionID, req.filePath, req.oldContent, req.writeContent); err != nil {
		return fantasy.ToolResponse{}, err
	}
	if req.wholeFileRead {
		recordWholeFileRead(req.ctx, req.filetracker, req.sessionID, req.filePath)
	} else {
		recordEditedSpan(req.ctx, req.filetracker, req.sessionID, req.filePath, req.oldContent, req.diffContent)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(req.successMessage),
		req.metadata(req.writeContent, diffText, additions, removals),
	), nil
}
