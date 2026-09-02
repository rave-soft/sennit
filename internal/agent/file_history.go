package agent

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/history"
)

// fileHistory adapts file history storage to the narrow port used by
// file-mutating tools. SQL's no-row sentinel is translated at this boundary.
type fileHistoryStore interface {
	CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error)
	GetByPathAndSession(ctx context.Context, path, sessionID string) (history.File, error)
}

type fileHistory struct {
	service fileHistoryStore
}

func newFileHistory(service fileHistoryStore) tools.FileHistory {
	if service == nil {
		return nil
	}
	return fileHistory{service: service}
}

func (h fileHistory) CreateVersion(ctx context.Context, sessionID, path, content string) error {
	_, err := h.service.CreateVersion(ctx, sessionID, path, content)
	return err
}

func (h fileHistory) LatestContent(ctx context.Context, sessionID, path string) (string, bool, error) {
	file, err := h.service.GetByPathAndSession(ctx, path, sessionID)
	if err == nil {
		return file.Content, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return "", false, err
}
