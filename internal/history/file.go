package history

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/pubsub"
)

const (
	InitialVersion = 0
)

type File struct {
	ID        string
	SessionID string
	Path      string
	Content   string
	Version   int64
	CreatedAt int64
	UpdatedAt int64
}

// Service manages file versions and history for sessions.
type Service interface {
	pubsub.Subscriber[File]
	Create(ctx context.Context, sessionID, path, content string) (File, error)

	// CreateVersion creates a new version of a file.
	CreateVersion(ctx context.Context, sessionID, path, content string) (File, error)

	Get(ctx context.Context, id string) (File, error)
	GetByPathAndSession(ctx context.Context, path, sessionID string) (File, error)
	ListBySession(ctx context.Context, sessionID string) ([]File, error)
	ListBySessionTree(ctx context.Context, sessionID string) ([]File, error)
	ListLatestSessionFiles(ctx context.Context, sessionID string) ([]File, error)
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
	*pubsub.Broker[File]
	q        *db.Queries
	versions fileVersionStore
}

func NewService(q *db.Queries, sqlDB *sql.DB) Service {
	return &service{
		Broker:   pubsub.NewBroker[File](),
		q:        q,
		versions: sqlFileVersionStore{db: sqlDB, q: q},
	}
}

func (s *service) Create(ctx context.Context, sessionID, path, content string) (File, error) {
	dbFile, err := s.q.CreateFile(ctx, db.CreateFileParams{
		ID:        uuid.New().String(),
		SessionID: sessionID,
		Path:      path,
		Content:   content,
		Version:   InitialVersion,
	})
	if err != nil {
		return File{}, err
	}

	file := s.fromDBItem(dbFile)
	s.Publish(pubsub.CreatedEvent, file)
	return file, nil
}

// CreateVersion creates a new version of a file with auto-incremented version
// number. If no previous versions exist for the path, it creates the initial
// version. The provided content is stored as the new version. Version
// numbers are global per path (shared across sessions, matching
// ListFilesByPath and ListLatestSessionFiles semantics), so the next version
// is computed inside the same transaction as the insert to avoid a
// read-then-write race between concurrent callers.
func (s *service) CreateVersion(ctx context.Context, sessionID, path, content string) (File, error) {
	tx, err := s.versions.Begin(ctx)
	if err != nil {
		return File{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nextVersion, err := tx.NextFileVersion(ctx, path)
	if err != nil {
		return File{}, fmt.Errorf("failed to determine next file version: %w", err)
	}

	dbFile, err := tx.CreateFile(ctx, db.CreateFileParams{
		ID:        uuid.New().String(),
		SessionID: sessionID,
		Path:      path,
		Content:   content,
		Version:   nextVersion,
	})
	if err != nil {
		return File{}, err
	}

	if err := tx.Commit(); err != nil {
		return File{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	file := s.fromDBItem(dbFile)
	s.Publish(pubsub.CreatedEvent, file)
	return file, nil
}

func (s *service) Get(ctx context.Context, id string) (File, error) {
	dbFile, err := s.q.GetFile(ctx, id)
	if err != nil {
		return File{}, err
	}
	return s.fromDBItem(dbFile), nil
}

func (s *service) GetByPathAndSession(ctx context.Context, path, sessionID string) (File, error) {
	dbFile, err := s.q.GetFileByPathAndSession(ctx, db.GetFileByPathAndSessionParams{
		Path:      path,
		SessionID: sessionID,
	})
	if err != nil {
		return File{}, err
	}
	return s.fromDBItem(dbFile), nil
}

func (s *service) ListBySession(ctx context.Context, sessionID string) ([]File, error) {
	dbFiles, err := s.q.ListFilesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(dbFiles))
	for i, dbFile := range dbFiles {
		files[i] = s.fromDBItem(dbFile)
	}
	return files, nil
}

// ListBySessionTree returns files from the root session and all of its
// descendants, regardless of which session in the tree was requested.
func (s *service) ListBySessionTree(ctx context.Context, sessionID string) ([]File, error) {
	dbFiles, err := s.q.ListFilesBySessionTree(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(dbFiles))
	for i, dbFile := range dbFiles {
		files[i] = s.fromDBItem(dbFile)
	}
	return files, nil
}

func (s *service) ListLatestSessionFiles(ctx context.Context, sessionID string) ([]File, error) {
	dbFiles, err := s.q.ListLatestSessionFiles(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(dbFiles))
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

func (s *service) DeleteSessionFiles(ctx context.Context, sessionID string) error {
	files, err := s.ListBySession(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, file := range files {
		err = s.Delete(ctx, file.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *service) fromDBItem(item db.File) File {
	return File{
		ID:        item.ID,
		SessionID: item.SessionID,
		Path:      item.Path,
		Content:   item.Content,
		Version:   item.Version,
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	}
}
