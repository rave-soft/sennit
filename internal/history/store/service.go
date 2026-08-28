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

	Get(ctx context.Context, id string) (history.File, error)
	GetByPathAndSession(ctx context.Context, path, sessionID string) (history.File, error)
	ListBySession(ctx context.Context, sessionID string) ([]history.File, error)
	ListBySessionTree(ctx context.Context, sessionID string) ([]history.File, error)
	ListLatestSessionFiles(ctx context.Context, sessionID string) ([]history.File, error)
	Delete(ctx context.Context, id string) error
	DeleteSessionFiles(ctx context.Context, sessionID string) error
}

type fileVersionTransaction interface {
	NextFileVersion(ctx context.Context, path string) (int64, error)
	CreateFile(ctx context.Context, params db.CreateFileParams) (db.File, error)
	Commit() error
	Rollback() error
}

type fileVersionStore interface {
	NextFileVersion(ctx context.Context, path string) (int64, error)
	Begin(ctx context.Context) (fileVersionTransaction, error)
}

type sqlFileVersionStore struct {
	db *sql.DB
	q  *db.Queries
}

func (s sqlFileVersionStore) NextFileVersion(ctx context.Context, path string) (int64, error) {
	return s.q.NextFileVersion(ctx, path)
}

func (s sqlFileVersionStore) Begin(ctx context.Context) (fileVersionTransaction, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &sqlFileVersionTransaction{Tx: tx, q: s.q.WithTx(tx)}, nil
}

type sqlFileVersionTransaction struct {
	*sql.Tx
	q *db.Queries
}

func (tx *sqlFileVersionTransaction) NextFileVersion(ctx context.Context, path string) (int64, error) {
	return tx.q.NextFileVersion(ctx, path)
}

func (tx *sqlFileVersionTransaction) CreateFile(ctx context.Context, params db.CreateFileParams) (db.File, error) {
	return tx.q.CreateFile(ctx, params)
}

type service struct {
	*pubsub.Broker[history.File]
	db       *sql.DB
	q        *db.Queries
	versions fileVersionStore
}

func NewService(q *db.Queries, sqlDB *sql.DB) Service {
	return &service{
		Broker:   pubsub.NewBroker[history.File](),
		db:       sqlDB,
		q:        q,
		versions: sqlFileVersionStore{db: sqlDB, q: q},
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
	tx, err := s.versions.Begin(ctx)
	if err != nil {
		return history.File{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nextVersion, err := tx.NextFileVersion(ctx, path)
	if err != nil {
		return history.File{}, fmt.Errorf("failed to determine next file version: %w", err)
	}

	dbFile, err := tx.CreateFile(ctx, db.CreateFileParams{
		ID:        uuid.New().String(),
		SessionID: sessionID,
		Path:      path,
		Content:   content,
		Version:   nextVersion,
	})
	if err != nil {
		return history.File{}, err
	}

	if err := tx.Commit(); err != nil {
		return history.File{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	file := s.fromDBItem(dbFile)
	s.Publish(pubsub.CreatedEvent, file)
	return file, nil
}

func (s *service) Get(ctx context.Context, id string) (history.File, error) {
	dbFile, err := s.q.GetFile(ctx, id)
	if err != nil {
		return history.File{}, err
	}
	return s.fromDBItem(dbFile), nil
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

func (s *service) ListBySession(ctx context.Context, sessionID string) ([]history.File, error) {
	dbFiles, err := s.q.ListFilesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]history.File, len(dbFiles))
	for i, dbFile := range dbFiles {
		files[i] = s.fromDBItem(dbFile)
	}
	return files, nil
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

func (s *service) ListLatestSessionFiles(ctx context.Context, sessionID string) ([]history.File, error) {
	dbFiles, err := s.q.ListLatestSessionFiles(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]history.File, len(dbFiles))
	for i, dbFile := range dbFiles {
		files[i] = s.fromDBItem(dbFile)
	}
	return files, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	file, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	err = s.q.DeleteFile(ctx, id)
	if err != nil {
		return err
	}
	s.Publish(pubsub.DeletedEvent, file)
	return nil
}

// DeleteSessionFiles deletes every file version belonging to sessionID as
// one transaction, so a failure partway through leaves none of them
// removed rather than an arbitrary prefix. Deletion events are published
// only after the transaction commits, once the rows are actually gone.
func (s *service) DeleteSessionFiles(ctx context.Context, sessionID string) error {
	files, err := s.ListBySession(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := db.InTx(ctx, s.db, func(qtx *db.Queries) error {
		for _, file := range files {
			if err := qtx.DeleteFile(ctx, file.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, file := range files {
		s.Publish(pubsub.DeletedEvent, file)
	}
	return nil
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
