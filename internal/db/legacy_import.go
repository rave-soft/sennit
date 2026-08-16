package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rave-soft/sennit/internal/brand"
)

// ImportLegacyProjectDB imports a pre-shared-database project's SQLite
// file (<projectDir>/sennit.db, from before every project moved to one
// shared database) into dest, stamping every row with projectPath, then
// renames the source file to sennit.db.imported so it is never imported
// again. It is a no-op if the legacy file doesn't exist or was already
// imported (i.e. already renamed). Rows with primary key or unique
// conflicts, and child rows orphaned by a missing session, are skipped
// with a warning; all other import errors roll back the import (and the
// import is retried on the next startup, since the file is not renamed).
func ImportLegacyProjectDB(ctx context.Context, projectDir, projectPath string, dest *sql.DB) error {
	legacyPath := filepath.Join(projectDir, brand.DBFile)
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		// Never had a legacy DB, or already imported and renamed away.
		return nil
	}

	// Connect runs the full migration chain on the legacy file, bringing
	// its schema (including project_path, defaulted to '') up to date
	// before we read from it.
	legacyConn, err := Connect(ctx, projectDir)
	if err != nil {
		return fmt.Errorf("failed to open legacy database: %w", err)
	}
	// Released explicitly (not deferred) below, before the rename, so the
	// file is fully closed and its WAL checkpointed first.
	released := false
	release := func() {
		if !released {
			released = true
			_ = Release(projectDir)
		}
	}
	defer release()

	counts, err := importLegacyRows(ctx, legacyConn, dest, projectPath)
	if err != nil {
		return fmt.Errorf("failed to import legacy database: %w", err)
	}

	release()

	// If the rename below fails, a later call repeats the import. Existing rows
	// are skipped on conflict, while any operational error still rolls back.
	if err := os.Rename(legacyPath, legacyPath+".imported"); err != nil {
		slog.Warn("Failed to rename legacy database after import", "path", legacyPath, "error", err)
	}

	// Best-effort cleanup of WAL sidecar files left behind by the legacy
	// connection; ignore errors, they're irrelevant once the main file is
	// renamed away.
	_ = os.Remove(legacyPath + "-wal")
	_ = os.Remove(legacyPath + "-shm")

	slog.Info("Imported legacy project database",
		"project_path", projectPath,
		"sessions_imported", counts.sessionsImported, "sessions_skipped", counts.sessionsSkipped,
		"messages_imported", counts.messagesImported, "messages_skipped", counts.messagesSkipped,
		"files_imported", counts.filesImported, "files_skipped", counts.filesSkipped,
		"read_files_imported", counts.readFilesImported, "read_files_skipped", counts.readFilesSkipped,
		"threads_imported", counts.threadsImported, "threads_skipped", counts.threadsSkipped,
	)
	return nil
}

// legacyImportCounts tallies what importLegacyRows did, for the closing
// summary log.
type legacyImportCounts struct {
	sessionsImported, sessionsSkipped   int
	messagesImported, messagesSkipped   int
	filesImported, filesSkipped         int
	readFilesImported, readFilesSkipped int
	threadsImported, threadsSkipped     int
}

// importLegacyRows copies every row from legacyConn's sessions, messages,
// files, read_files, and threads tables into dest, all within a single
// transaction on dest, stamping projectPath onto sessions and threads.
func importLegacyRows(ctx context.Context, legacyConn, dest *sql.DB, projectPath string) (legacyImportCounts, error) {
	var counts legacyImportCounts

	tx, err := dest.BeginTx(ctx, nil)
	if err != nil {
		return counts, fmt.Errorf("failed to begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	skippedSessions := make(map[string]bool)
	importedSessions := make(map[string]int64)
	// Every session ID present in the legacy file. Child rows referencing
	// a session outside this set are orphans (e.g. left behind by an old
	// crash with foreign keys off); inserting them into dest would fail
	// its FOREIGN KEY enforcement and roll back the whole import — which
	// would then be retried and fail identically on every startup. Skip
	// them like conflicts instead: an orphan row was unreachable in the
	// legacy database too.
	knownSessions := make(map[string]bool)

	sessionRows, err := legacyConn.QueryContext(ctx, `SELECT id, parent_session_id, title, message_count, prompt_tokens, completion_tokens, cost, updated_at, created_at, summary_message_id, todos FROM sessions`)
	if err != nil {
		return counts, fmt.Errorf("failed to read legacy sessions: %w", err)
	}
	defer sessionRows.Close()
	type legacySession struct {
		id, title                                    string
		parentSessionID, summaryMessageID, todos     sql.NullString
		messageCount, promptTokens, completionTokens int64
		updatedAt, createdAt                         int64
		cost                                         float64
	}
	var sessions []legacySession
	for sessionRows.Next() {
		var s legacySession
		if err := sessionRows.Scan(&s.id, &s.parentSessionID, &s.title, &s.messageCount, &s.promptTokens, &s.completionTokens, &s.cost, &s.updatedAt, &s.createdAt, &s.summaryMessageID, &s.todos); err != nil {
			return counts, fmt.Errorf("failed to scan legacy session: %w", err)
		}
		sessions = append(sessions, s)
	}
	if err := sessionRows.Err(); err != nil {
		return counts, fmt.Errorf("failed to read legacy sessions: %w", err)
	}

	for _, s := range sessions {
		knownSessions[s.id] = true
		result, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(id, parent_session_id, title, message_count, prompt_tokens, completion_tokens, cost, updated_at, created_at, summary_message_id, todos, project_path)
			VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			s.id, s.parentSessionID, s.title, s.promptTokens, s.completionTokens, s.cost, s.updatedAt, s.createdAt, s.summaryMessageID, s.todos, projectPath)
		if err != nil {
			return counts, fmt.Errorf("failed to import legacy session %q: %w", s.id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return counts, fmt.Errorf("failed to determine legacy session import result %q: %w", s.id, err)
		}
		if affected == 0 {
			slog.Warn("Skipping legacy session on import conflict", "session_id", s.id, "project_path", projectPath)
			skippedSessions[s.id] = true
			counts.sessionsSkipped++
			continue
		}
		counts.sessionsImported++
		importedSessions[s.id] = s.updatedAt
	}

	if err := importLegacyMessages(ctx, legacyConn, tx, skippedSessions, knownSessions, &counts); err != nil {
		return counts, err
	}
	if err := importLegacyFiles(ctx, legacyConn, tx, skippedSessions, knownSessions, &counts); err != nil {
		return counts, err
	}
	if err := importLegacyReadFiles(ctx, legacyConn, tx, skippedSessions, knownSessions, &counts); err != nil {
		return counts, err
	}
	if err := importLegacyThreads(ctx, legacyConn, tx, projectPath, skippedSessions, knownSessions, &counts); err != nil {
		return counts, err
	}
	// Restore updated_at values the message-count trigger cascade bumped
	// during the child-row inserts above. The `updated_at <> ?` guard is
	// load-bearing: the auto-bump trigger fires exactly WHEN NEW = OLD, so
	// an identity UPDATE (a session whose timestamp was never bumped, e.g.
	// zero messages imported) would be rewritten to now() by the trigger.
	// Skipping the unchanged rows both avoids that and keeps the trigger's
	// explicit-write exemption (NEW <> OLD) for the rows that do need it.
	for id, updatedAt := range importedSessions {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ? AND updated_at <> ?`, updatedAt, id, updatedAt); err != nil {
			return counts, fmt.Errorf("failed to restore legacy session updated_at %q: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return counts, fmt.Errorf("failed to commit import transaction: %w", err)
	}
	committed = true

	return counts, nil
}

func importLegacyMessages(ctx context.Context, legacyConn *sql.DB, tx *sql.Tx, skippedSessions, knownSessions map[string]bool, counts *legacyImportCounts) error {
	rows, err := legacyConn.QueryContext(ctx, `SELECT id, session_id, role, parts, model, created_at, updated_at, finished_at, provider, is_summary_message FROM messages`)
	if err != nil {
		return fmt.Errorf("failed to read legacy messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, sessionID, role, parts string
		var model, provider sql.NullString
		var createdAt, updatedAt int64
		var finishedAt sql.NullInt64
		var isSummaryMessage int64
		if err := rows.Scan(&id, &sessionID, &role, &parts, &model, &createdAt, &updatedAt, &finishedAt, &provider, &isSummaryMessage); err != nil {
			return fmt.Errorf("failed to scan legacy message: %w", err)
		}
		if skippedSessions[sessionID] {
			counts.messagesSkipped++
			continue
		}
		if !knownSessions[sessionID] {
			slog.Warn("Skipping orphaned legacy message (no such session)", "message_id", id, "session_id", sessionID)
			counts.messagesSkipped++
			continue
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO messages
			(id, session_id, role, parts, model, created_at, updated_at, finished_at, provider, is_summary_message)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			id, sessionID, role, parts, model, createdAt, updatedAt, finishedAt, provider, isSummaryMessage)
		if err != nil {
			return fmt.Errorf("failed to import legacy message %q: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to determine legacy message import result %q: %w", id, err)
		}
		if affected == 0 {
			slog.Warn("Skipping legacy message on import conflict", "message_id", id)
			counts.messagesSkipped++
			continue
		}
		counts.messagesImported++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read legacy messages: %w", err)
	}
	return nil
}

func importLegacyFiles(ctx context.Context, legacyConn *sql.DB, tx *sql.Tx, skippedSessions, knownSessions map[string]bool, counts *legacyImportCounts) error {
	rows, err := legacyConn.QueryContext(ctx, `SELECT id, session_id, path, content, version, created_at, updated_at FROM files`)
	if err != nil {
		return fmt.Errorf("failed to read legacy files: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, sessionID, path, content string
		var version, createdAt, updatedAt int64
		if err := rows.Scan(&id, &sessionID, &path, &content, &version, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("failed to scan legacy file: %w", err)
		}
		if skippedSessions[sessionID] {
			counts.filesSkipped++
			continue
		}
		if !knownSessions[sessionID] {
			slog.Warn("Skipping orphaned legacy file (no such session)", "file_id", id, "session_id", sessionID)
			counts.filesSkipped++
			continue
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO files
			(id, session_id, path, content, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			id, sessionID, path, content, version, createdAt, updatedAt)
		if err != nil {
			return fmt.Errorf("failed to import legacy file %q: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to determine legacy file import result %q: %w", id, err)
		}
		if affected == 0 {
			slog.Warn("Skipping legacy file on import conflict", "file_id", id)
			counts.filesSkipped++
			continue
		}
		counts.filesImported++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read legacy files: %w", err)
	}
	return nil
}

func importLegacyReadFiles(ctx context.Context, legacyConn *sql.DB, tx *sql.Tx, skippedSessions, knownSessions map[string]bool, counts *legacyImportCounts) error {
	rows, err := legacyConn.QueryContext(ctx, `SELECT session_id, path, read_at FROM read_files`)
	if err != nil {
		return fmt.Errorf("failed to read legacy read_files: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID, path string
		var readAt int64
		if err := rows.Scan(&sessionID, &path, &readAt); err != nil {
			return fmt.Errorf("failed to scan legacy read_files row: %w", err)
		}
		if skippedSessions[sessionID] {
			counts.readFilesSkipped++
			continue
		}
		if !knownSessions[sessionID] {
			slog.Warn("Skipping orphaned legacy read_files row (no such session)", "session_id", sessionID, "path", path)
			counts.readFilesSkipped++
			continue
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO read_files (session_id, path, read_at)
			VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, sessionID, path, readAt)
		if err != nil {
			return fmt.Errorf("failed to import legacy read_files row for session %q path %q: %w", sessionID, path, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to determine legacy read_files import result for session %q path %q: %w", sessionID, path, err)
		}
		if affected == 0 {
			slog.Warn("Skipping legacy read_files row on import conflict", "session_id", sessionID, "path", path)
			counts.readFilesSkipped++
			continue
		}
		counts.readFilesImported++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read legacy read_files: %w", err)
	}
	return nil
}

func importLegacyThreads(ctx context.Context, legacyConn *sql.DB, tx *sql.Tx, projectPath string, skippedSessions, knownSessions map[string]bool, counts *legacyImportCounts) error {
	// legacyConn already ran the full migration chain (see
	// ImportLegacyProjectDB), so the threads table always exists here,
	// even for a legacy file that predated it.
	rows, err := legacyConn.QueryContext(ctx, `SELECT id, name, goal, base_branch, branch, worktree_path, session_id, status, merge_policy, result_summary, error, created_at, updated_at, completed_at FROM threads`)
	if err != nil {
		return fmt.Errorf("failed to read legacy threads: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, name, goal, baseBranch, branch, worktreePath, sessionID, status, mergePolicy, resultSummary, errCol string
		var createdAt, updatedAt int64
		var completedAt sql.NullInt64
		if err := rows.Scan(&id, &name, &goal, &baseBranch, &branch, &worktreePath, &sessionID, &status, &mergePolicy, &resultSummary, &errCol, &createdAt, &updatedAt, &completedAt); err != nil {
			return fmt.Errorf("failed to scan legacy thread: %w", err)
		}
		if sessionID != "" && skippedSessions[sessionID] {
			counts.threadsSkipped++
			continue
		}
		if sessionID != "" && !knownSessions[sessionID] {
			slog.Warn("Skipping orphaned legacy thread (no such session)", "thread_id", id, "session_id", sessionID)
			counts.threadsSkipped++
			continue
		}
		// project_path is stamped with the current project, overriding
		// whatever the legacy migration defaulted it to ('').
		result, err := tx.ExecContext(ctx, `INSERT INTO threads
			(id, name, project_path, goal, base_branch, branch, worktree_path, session_id, status, merge_policy, result_summary, error, created_at, updated_at, completed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			id, name, projectPath, goal, baseBranch, branch, worktreePath, sessionID, status, mergePolicy, resultSummary, errCol, createdAt, updatedAt, completedAt)
		if err != nil {
			return fmt.Errorf("failed to import legacy thread %q: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to determine legacy thread import result %q: %w", id, err)
		}
		if affected == 0 {
			slog.Warn("Skipping legacy thread on import conflict", "thread_id", id, "project_path", projectPath)
			counts.threadsSkipped++
			continue
		}
		counts.threadsImported++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read legacy threads: %w", err)
	}
	return nil
}
