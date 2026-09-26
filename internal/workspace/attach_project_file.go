package workspace

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rave-soft/sennit/internal/message"
)

// Errors AttachProjectFile returns for the outcomes internal/ui/model's old
// editor_input.go handled with different user-visible behavior before this
// logic moved server-side: a missing file or a directory says nothing (the
// model can still read it later with a tool), an oversized or
// unsupported-type file gets a warning naming why, and a read failure that
// survives the earlier stat is treated the same as "let the model handle
// it" always was. Callers should prefer errors.Is over comparing err
// directly, since AttachProjectFileUsing wraps these with the path.
var (
	// ErrAttachFileMissing means path could not be stat'd (removed since
	// it was listed, or never existed).
	ErrAttachFileMissing = errors.New("file not found")
	// ErrAttachIsDirectory means path names a directory, not a file.
	ErrAttachIsDirectory = errors.New("path is a directory")
	// ErrAttachTooBig means the file exceeds message.MaxAttachmentSize.
	ErrAttachTooBig = errors.New("file is too big to attach")
	// ErrAttachUnsupportedType means the file's sniffed MIME type is
	// neither text nor an image.
	ErrAttachUnsupportedType = errors.New("attachments must be text or images")
	// ErrAttachReadFailed means the file passed every check above but
	// could not be read.
	ErrAttachReadFailed = errors.New("failed to read file")
)

// AttachProjectFileUsing is AttachProjectFile's logic, taking its file-
// tracker lookup as a parameter so it can be tested without a real
// [filetracker.Service] or database — see appws.AppWorkspace.
// AttachProjectFile, its only production caller, which passes
// w.app.FileTracker.LastReadTime bound to its own session store.
//
// root is the workspace's working directory; a relative path is resolved
// against it (not the caller's process cwd — the point of moving this out
// of internal/ui). sessionID may be empty, meaning no session exists yet
// (mirrors the old editor_input.go behavior of skipping the file-tracker
// check before a session exists at all; the client keeps its own
// pre-session dedupe against repeated picks, since that list is UI state
// with no session to key it by).
//
// The bool return means "already read by the agent and unchanged since":
// true only when sessionID is non-empty, the file tracker has a read
// recorded, and the file's mtime is no later than that. It is not an
// error - the query text is still inserted, just without an attachment.
func AttachProjectFileUsing(
	ctx context.Context,
	root, sessionID, path string,
	lastReadTime func(ctx context.Context, sessionID, path string) (time.Time, error),
) (message.Attachment, bool, error) {
	absPath := path
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(root, path)
	}

	if sessionID != "" {
		lastRead, err := lastReadTime(ctx, sessionID, absPath)
		if err != nil {
			slog.Warn("Failed to read last-read time for file", "session_id", sessionID, "path", absPath, "error", err)
		}
		if !lastRead.IsZero() {
			if info, err := os.Stat(absPath); err == nil && !info.ModTime().After(lastRead) {
				return message.Attachment{}, true, nil
			}
		}
	}

	// Stat before reading: the @ completion list enumerates every file
	// regardless of size or type, so an oversized pick (a .sqlite, a
	// .pack, a video) or a directory must be caught before it is read
	// whole into memory.
	info, err := os.Stat(absPath)
	if err != nil {
		return message.Attachment{}, false, ErrAttachFileMissing
	}
	if info.IsDir() {
		return message.Attachment{}, false, ErrAttachIsDirectory
	}
	if info.Size() > message.MaxAttachmentSize {
		return message.Attachment{}, false, ErrAttachTooBig
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return message.Attachment{}, false, ErrAttachReadFailed
	}

	mimeType := sniffMimeType(content)
	if !strings.HasPrefix(mimeType, "text/") && !strings.HasPrefix(mimeType, "image/") {
		return message.Attachment{}, false, ErrAttachUnsupportedType
	}

	return message.Attachment{
		FilePath: path,
		FileName: filepath.Base(path),
		MimeType: mimeType,
		Content:  content,
	}, false, nil
}

// sniffMimeType mirrors internal/ui/model's old mimeOf helper.
func sniffMimeType(content []byte) string {
	n := min(512, len(content))
	return http.DetectContentType(content[:n])
}
