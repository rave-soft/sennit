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
	case modTime.Truncate(time.Second).After(lastRead):
		return fileStale, lastRead
	default:
		return fileFresh, lastRead
	}
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
	snapshot, err := readFileSnapshot(req.filePath)
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
	resp, denied, err := requirePermission(req.ctx, req.permissions, permission.CreatePermissionRequest{
		SessionID: req.sessionID, Path: fsext.PathOrPrefix(req.filePath, req.workingDir), ToolCallID: req.call.ID,
		ToolName: req.toolName, Action: "write", Description: prepared.description, Params: prepared.permParams,
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if denied {
		return fantasy.WithResponseMetadata(resp, prepared.metadata(prepared.diffContent, diffText, additions, removals)), nil
	}
	if !snapshot.exists {
		if err := ensureParentDir(req.filePath); err != nil {
			return fantasy.ToolResponse{}, err
		}
	}
	mode := snapshot.mode
	if !snapshot.exists {
		mode = 0o644
	}
	err = fsext.AtomicWriteFileIfUnchanged(req.filePath, snapshot.raw, []byte(prepared.writeContent), mode, snapshot.exists)
	if err != nil {
		if errors.Is(err, fsext.ErrFileChanged) {
			return fantasy.NewTextErrorResponse("file changed on disk after approval; retry after reading the current file"), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("failed to atomically write file: %w", err)
	}
	historyErr := recordFileHistory(req.ctx, req.files, req.sessionID, req.filePath, string(snapshot.raw), prepared.writeContent)
	if req.filetracker != nil {
		if prepared.wholeFileRead {
			recordWholeFileRead(req.ctx, req.filetracker, req.sessionID, req.filePath)
		} else {
			recordEditedSpan(req.ctx, req.filetracker, req.sessionID, req.filePath, snapshot.content, prepared.diffContent)
		}
	}
	if historyErr != nil {
		return fantasy.ToolResponse{}, &committedMutationError{err: fmt.Errorf("file committed but history update failed: %w", historyErr)}
	}
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(prepared.successMessage), prepared.metadata(prepared.writeContent, diffText, additions, removals)), nil
}
