package tools

import "context"

// FileHistory records versions needed to preserve a file mutation's undo
// history. It intentionally exposes only the tool layer's write-time needs.
type FileHistory interface {
	CreateVersion(ctx context.Context, sessionID, path, content string) error
	LatestContent(ctx context.Context, sessionID, path string) (content string, found bool, err error)
}
