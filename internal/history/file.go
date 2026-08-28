// Package history models the recorded versions of files a session (or its
// sub-agents) touched, so the TUI can render diffs and revert-to-version
// without depending on the SQLite service that persists them.
package history

// File is one recorded version of a file, attached to the session that
// wrote it.
type File struct {
	ID        string
	SessionID string
	Path      string
	Content   string
	Version   int64
	CreatedAt int64
	UpdatedAt int64
}
