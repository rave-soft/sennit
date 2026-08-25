// Package filetracker provides functionality to track file reads in sessions.
package filetracker

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/rave-soft/sennit/internal/db"
)

// Service defines the interface for tracking file reads in sessions.
type Service interface {
	// RecordRead records that the whole file was read.
	RecordRead(ctx context.Context, sessionID, path string)

	// RecordPartialRead records that lines [start, end] of the file were
	// read — the read tool serves windows, and an edit is only allowed to
	// touch lines that were actually served. Ranges accumulate across
	// reads of the same file.
	RecordPartialRead(ctx context.Context, sessionID, path string, start, end int)

	// RecordEdit records an edit that replaced the file's lines
	// [start, end] with newEnd-start+1 of them: the edited span becomes
	// covered (the session was just shown that text), and everything
	// recorded below the edit is renumbered to follow the lines it
	// describes. It does not widen coverage to the whole file — an edit
	// teaches the session about the region it touched, nothing more.
	RecordEdit(ctx context.Context, sessionID, path string, start, end, newEnd int)

	// ReadCoverage returns which lines of the file this session has read.
	ReadCoverage(ctx context.Context, sessionID, path string) Coverage

	// LastReadTime returns when a file was last read.
	// Returns zero time if never read.
	LastReadTime(ctx context.Context, sessionID, path string) time.Time

	// ListReadFiles returns the paths of all files read in a session.
	ListReadFiles(ctx context.Context, sessionID string) ([]string, error)
}

type service struct {
	q          *db.Queries
	workingDir string
}

// NewService creates a new file tracker service. Paths recorded and
// resolved by the service are relative to workingDir — normally the
// workspace's working directory, not the process's os.Getwd(). This
// matters in server mode, where the process cwd need not match the
// workspace being served.
func NewService(q *db.Queries, workingDir string) Service {
	return &service{q: q, workingDir: workingDir}
}

// RecordRead records that the whole file was read, superseding whatever
// partial ranges were recorded before.
func (s *service) RecordRead(ctx context.Context, sessionID, path string) {
	s.record(ctx, sessionID, path, FullCoverage)
}

// RecordPartialRead records a window of the file as read, merged into
// whatever this session had already seen.
func (s *service) RecordPartialRead(ctx context.Context, sessionID, path string, start, end int) {
	path = s.relpath(path)
	s.update(ctx, sessionID, path, func(encoded string) string {
		return encodeRanges(decodeRanges(encoded).Add(LineRange{Start: start, End: end}))
	})
}

// RecordEdit records the span an edit replaced, renumbering the coverage
// below it.
func (s *service) RecordEdit(ctx context.Context, sessionID, path string, start, end, newEnd int) {
	path = s.relpath(path)
	s.update(ctx, sessionID, path, func(encoded string) string {
		coverage := decodeRanges(encoded)
		coverage = coverage.Shift(start, end, newEnd-end).Add(LineRange{Start: start, End: newEnd})
		return encodeRanges(coverage)
	})
}

// ReadCoverage returns which lines of the file this session has read.
func (s *service) ReadCoverage(ctx context.Context, sessionID, path string) Coverage {
	readFile, err := s.q.GetFileRead(ctx, db.GetFileReadParams{
		SessionID: sessionID,
		Path:      s.relpath(path),
	})
	if err != nil {
		return Coverage{}
	}
	return decodeRanges(readFile.ReadRanges)
}

func (s *service) update(ctx context.Context, sessionID, path string, update func(string) string) {
	if err := s.q.UpdateFileRead(ctx, sessionID, path, update); err != nil {
		slog.Error("Error recording file read", "error", err, "file", path)
	}
}

func (s *service) record(ctx context.Context, sessionID, path string, coverage Coverage) {
	if err := s.q.RecordFileRead(ctx, db.RecordFileReadParams{
		SessionID:  sessionID,
		Path:       s.relpath(path),
		ReadRanges: encodeRanges(coverage),
	}); err != nil {
		slog.Error("Error recording file read", "error", err, "file", path)
	}
}

// LastReadTime returns when a file was last read.
// Returns zero time if never read.
func (s *service) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	readFile, err := s.q.GetFileRead(ctx, db.GetFileReadParams{
		SessionID: sessionID,
		Path:      s.relpath(path),
	})
	if err != nil {
		return time.Time{}
	}

	return time.Unix(readFile.ReadAt, 0)
}

func (s *service) relpath(path string) string {
	path = filepath.Clean(path)
	relpath, err := filepath.Rel(s.workingDir, path)
	if err != nil {
		slog.Warn("Error getting relpath", "error", err)
		return path
	}
	return relpath
}

// ListReadFiles returns the paths of all files read in a session.
func (s *service) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	readFiles, err := s.q.ListSessionReadFiles(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing read files: %w", err)
	}

	paths := make([]string, 0, len(readFiles))
	for _, rf := range readFiles {
		paths = append(paths, filepath.Join(s.workingDir, rf.Path))
	}
	return paths, nil
}
