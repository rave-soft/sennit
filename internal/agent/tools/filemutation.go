package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/diff"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
)

type fileFreshness int

const (
	fileFresh fileFreshness = iota
	fileNeverRead
	fileStale
)

func checkFileFreshness(ctx context.Context, ft FileTracking, sessionID, filePath string, modTime time.Time) (state fileFreshness, lastRead time.Time) {
	if ft == nil {
		return fileFresh, time.Time{}
	}
	lastRead = ft.LastReadTime(ctx, sessionID, filePath)
	switch {
	case lastRead.IsZero():
		return fileNeverRead, lastRead
	// read_at is millisecond precision, so modTime is truncated to
	// milliseconds before comparing: without it, a write the agent itself
	// just made — landing in the same millisecond as, but numerically
	// after, the row update that recorded the read — would report stale
	// and block the agent's own follow-up edit. Truncating to the
	// column's own resolution keeps read-then-edit within a millisecond
	// fresh; the cost is that a write inside that same millisecond is not
	// caught either, the millisecond-scale echo of the whole-second window
	// this resolution replaced. This works on filesystems with sub-second
	// mtime resolution (ext4, btrfs, APFS, NTFS); on a filesystem that
	// floors mtime to the second, a sub-second external write within the
	// same second as the read is still invisible here, because the
	// file's own recorded mtime cannot express it.
	case modTime.Truncate(time.Millisecond).After(lastRead):
		return fileStale, lastRead
	default:
		return fileFresh, lastRead
	}
}

// staleFileRefusal builds the refusal returned when a mutation targets a
// file that changed on disk after this session last read it. edit.go and
// write.go each hit this case with their own wording for why the
// staleness matters to that operation, so the differing halves — the verb,
// the "since when" clause, the rationale, and the closing instruction —
// are supplied by the caller and only the shared skeleton lives here.
func staleFileRefusal(filePath, verb, afterClause, rationale, instruction string, modTime, lastRead time.Time) string {
	return fmt.Sprintf(
		"cannot %s %s: it changed on disk %s "+
			"(modified %s, last read %s).\n\n"+
			"%s\n\n"+
			"%s",
		verb, filePath, afterClause,
		modTime.Truncate(time.Second).Format(time.RFC3339), lastRead.Format(time.RFC3339),
		rationale, instruction,
	)
}

type mutationStop struct {
	Response fantasy.ToolResponse
}

func (e *mutationStop) Error() string { return "file mutation stopped: " + e.Response.Content }

func stopWith(resp fantasy.ToolResponse) error { return &mutationStop{Response: resp} }

type fileSnapshot struct {
	raw     []byte
	content string
	mode    os.FileMode
	modTime time.Time
	exists  bool
	isDir   bool
	isCRLF  bool
}

type preparedFileMutation struct {
	diffContent    string
	writeContent   string
	wholeFileRead  bool
	description    string
	permParams     any
	successMessage string
	metadata       func(content, diffText string, additions, removals int) any
}

type fileMutationRequest struct {
	editContext
	call      fantasy.ToolCall
	filePath  string
	sessionID string
	toolName  string
	prepare   func(fileSnapshot) (preparedFileMutation, error)
}

func readFileSnapshot(filePath string) (fileSnapshot, error) {
	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil
	}
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("open file snapshot: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("stat file snapshot: %w", err)
	}
	if info.IsDir() {
		return fileSnapshot{mode: info.Mode(), modTime: info.ModTime(), exists: true, isDir: true}, nil
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("read file snapshot: %w", err)
	}
	content, isCRLF := fsext.ToUnixLineEndings(string(raw))
	return fileSnapshot{raw: raw, content: content, mode: info.Mode(), modTime: info.ModTime(), exists: true, isCRLF: isCRLF}, nil
}

type committedMutationError struct {
	err error
}

func (e *committedMutationError) Error() string { return e.err.Error() }
func (e *committedMutationError) Unwrap() error { return e.err }

func mutationCommitted(err error) bool {
	var committed *committedMutationError
	return errors.As(err, &committed)
}

func applyFileMutation(req fileMutationRequest) (fantasy.ToolResponse, error) {
	if msg, refused := confinementRefusal(req.permissions, req.filePath); refused {
		return fantasy.NewTextErrorResponse(msg), nil
	}
	// writePath is the file this mutation actually reads, compares, and
	// writes: req.filePath itself, or, when req.filePath is a symlink, the
	// file at the end of its target chain — the same file the diff and the
	// permission prompt already show, since readFileSnapshot below follows
	// the link the same way opening req.filePath directly always did. A
	// symlink is never renamed over: only writePath is, so the link itself
	// is left exactly as it was, and a dangling link's absent target is
	// simply where the file gets created.
	writePath, err := fsext.ResolveWriteTarget(req.filePath)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	snapshot, err := readFileSnapshot(writePath)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	prepared, err := req.prepare(snapshot)
	if err != nil {
		var stop *mutationStop
		if errors.As(err, &stop) {
			return stop.Response, nil
		}
		return fantasy.ToolResponse{}, err
	}
	diffText, additions, removals := diff.GenerateDiff(snapshot.content, prepared.diffContent, strings.TrimPrefix(req.filePath, req.workingDir))
	// The persistent-grant key is built from the symlink-resolved forms of
	// both the file and workingDir, not the plain ones PathOrPrefix would
	// otherwise compare: req.filePath can reach outside workingDir through
	// an ancestor directory symlink (writePath above only follows a
	// symlink at the leaf, so it does not catch this), and a grant keyed
	// on the unresolved path would survive that link being repointed to a
	// different destination. Resolving workingDir the same way keeps an
	// ordinary in-workdir write's key collapsed to workingDir exactly as
	// before whenever neither path involves a symlink.
	resolvedFilePath, err := resolveExistingAncestorSymlinks(req.filePath)
	if err != nil {
		resolvedFilePath = req.filePath
	}
	resolvedWorkingDir, err := resolveExistingAncestorSymlinks(req.workingDir)
	if err != nil {
		resolvedWorkingDir = req.workingDir
	}
	resp, denied, err := requirePermission(req.ctx, req.permissions, permission.CreatePermissionRequest{
		SessionID: req.sessionID, Path: fsext.PathOrPrefix(resolvedFilePath, resolvedWorkingDir), ToolCallID: req.call.ID,
		ToolName: req.toolName, Action: "write", Description: prepared.description, Params: prepared.permParams,
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if denied {
		return fantasy.WithResponseMetadata(resp, prepared.metadata(prepared.diffContent, diffText, additions, removals)), nil
	}
	if !snapshot.exists {
		if err := ensureParentDir(writePath); err != nil {
			return fantasy.ToolResponse{}, err
		}
	}
	mode := snapshot.mode
	if !snapshot.exists {
		mode = 0o644
	}
	err = fsext.AtomicWriteFileIfUnchanged(writePath, snapshot.raw, []byte(prepared.writeContent), mode, snapshot.exists)
	if err != nil {
		if errors.Is(err, fsext.ErrFileChanged) {
			return fantasy.NewTextErrorResponse("file changed on disk after approval; retry after reading the current file"), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("failed to atomically write file: %w", err)
	}
	historyErr := recordFileHistory(req.ctx, req.files, req.sessionID, writePath, string(snapshot.raw), prepared.writeContent)
	if req.filetracker != nil {
		if prepared.wholeFileRead {
			recordWholeFileRead(req.ctx, req.filetracker, req.sessionID, writePath)
		} else {
			recordEditedSpan(req.ctx, req.filetracker, req.sessionID, writePath, snapshot.content, prepared.diffContent)
		}
	}
	if historyErr != nil {
		return fantasy.ToolResponse{}, &committedMutationError{err: fmt.Errorf("file committed but history update failed: %w", historyErr)}
	}
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(prepared.successMessage), prepared.metadata(prepared.writeContent, diffText, additions, removals)), nil
}

// finishMutation runs the tail every mutating tool performs once
// applyFileMutation has returned: an outright failure (mutationCommitted
// is false) propagates as a Go error without touching the LSPs; a
// model-visible error response passes through untouched; otherwise the
// LSPs are notified of the write and the response comes back with wrap
// applied to its body plus diagnostics appended.
//
// The four callers agreed on everything but one detail: three returned a
// nil Go error alongside a model-visible error response, while
// lsp_replace_symbol returned err there. That difference is unreachable —
// applyFileMutation only ever pairs a committed-mutation error with a
// successful response — so this settles on the majority form, which is
// also the one the error-classification rule wants: an error the model can
// read is not also a batch-aborting Go error.
//
// wrap covers the one place the four callers differ — how the successful
// body gets framed ("<result>...</result>\n" for edit/multiedit,
// "<result>...</result>" for write, a computed summary for
// lsp_replace_symbol, which ignores the passed-in content and returns its
// own) — everything else here is identical across them.
func finishMutation(ctx context.Context, lspManager *lsp.Manager, filePath string, resp fantasy.ToolResponse, err error, wrap func(content string) string) (fantasy.ToolResponse, error) {
	if err != nil && !mutationCommitted(err) {
		return resp, err
	}
	if resp.IsError {
		return resp, nil
	}
	notifyLSPs(ctx, lspManager, filePath)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	resp.Content = wrap(resp.Content) + getDiagnostics(filePath, lspManager)
	return resp, nil
}
