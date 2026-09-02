// Package store is the SQLite-backed [Service] behind internal/history's
// [history.File] model. It is split out from internal/history so that
// package can stay free of database/sql and internal/db — internal/ui
// imports internal/history for the File type alone, and linking sqlc
// through it would drag both SQLite drivers along for the ride.
package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// Service manages file versions and history for sessions.
type Service interface {
	pubsub.Subscriber[history.File]

	// CreateVersion records content as the next version of path. It is
	// the only way to add a row: an "initial" version is just the first
	// one allocated for that path, and writing a fixed version 0 for it
	// would collide with whatever version another session already holds.
	CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error)

	GetByPathAndSession(ctx context.Context, path, sessionID string) (history.File, error)
	ListBySessionTree(ctx context.Context, sessionID string) ([]history.File, error)
}

type service struct {
	*pubsub.Broker[history.File]
	db *sql.DB
	q  *db.Queries
}

func NewService(q *db.Queries, sqlDB *sql.DB) Service {
	return &service{
		Broker: pubsub.NewBroker[history.File](),
		db:     sqlDB,
		q:      q,
	}
}

// CreateVersion creates a new version of a file with auto-incremented version
// number. If no previous versions exist for the path, it creates the initial
// version. The provided content is stored as the new version. Version
// numbers are global per path — shared across sessions, which is what lets
// ListBySessionTree order one file's versions across a whole session tree
// and is enforced by UNIQUE(path, version) — so the next version is
// computed inside the same transaction as the insert to avoid a
// read-then-write race between concurrent callers.
func (s *service) CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error) {
	var dbFile db.File
	if err := db.InTx(ctx, s.db, func(qtx *db.Queries) error {
		nextVersion, err := qtx.NextFileVersion(ctx, path)
		if err != nil {
			return fmt.Errorf("failed to determine next file version: %w", err)
		}

		dbFile, err = qtx.CreateFile(ctx, db.CreateFileParams{
			ID:        uuid.New().String(),
			SessionID: sessionID,
			Path:      path,
			Content:   content,
			Version:   nextVersion,
		})
		return err
	}); err != nil {
		return history.File{}, err
	}

	file := s.fromDBItem(dbFile)
	s.Publish(pubsub.CreatedEvent, file)
	return file, nil
}

func (s *service) GetByPathAndSession(ctx context.Context, path, sessionID string) (history.File, error) {
	dbFile, err := s.q.GetFileByPathAndSession(ctx, db.GetFileByPathAndSessionParams{
		Path:      path,
		SessionID: sessionID,
	})
	if err != nil {
		return history.File{}, err
	}
	return s.fromDBItem(dbFile), nil
}

// ListBySessionTree returns files from the root session and all of its
// descendants, regardless of which session in the tree was requested.
func (s *service) ListBySessionTree(ctx context.Context, sessionID string) ([]history.File, error) {
	dbFiles, err := s.q.ListFilesBySessionTree(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]history.File, len(dbFiles))
	for i, dbFile := range dbFiles {
		files[i] = s.fromDBItem(dbFile)
	}
	return files, nil
}

func (s *service) fromDBItem(item db.File) history.File {
	return history.File{
		ID:        item.ID,
		SessionID: item.SessionID,
		Path:      item.Path,
		Content:   item.Content,
		Version:   item.Version,
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	}
}
