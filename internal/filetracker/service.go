// Package filetracker provides functionality to track file reads in sessions.
package filetracker

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/rave-soft/braid/internal/db"
)

// Service defines the interface for tracking file reads in sessions.
type Service interface {
	// RecordRead records when a file was read.
	RecordRead(ctx context.Context, sessionID, path string)

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

// RecordRead records when a file was read.
func (s *service) RecordRead(ctx context.Context, sessionID, path string) {
	if err := s.q.RecordFileRead(ctx, db.RecordFileReadParams{
		SessionID: sessionID,
		Path:      s.relpath(path),
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
