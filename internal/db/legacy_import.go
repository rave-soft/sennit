package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// ImportLegacyProjectDB imports a pre-shared-database project's SQLite
// file (<projectDir>/braid.db, from before every project moved to one
// shared database) into dest, stamping every row with projectPath, then
// renames the source file to braid.db.imported so it is never imported
// again. It is a no-op if the legacy file doesn't exist or was already
// imported (i.e. already renamed). Best-effort per row: a row whose id
// collides with one already in dest (vanishingly unlikely for UUIDs, but
// not impossible) is skipped with a warning log rather than aborting the
// whole import.
func ImportLegacyProjectDB(ctx context.Context, projectDir, projectPath string, dest *sql.DB) error {
	legacyPath := filepath.Join(projectDir, "braid.db")
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

	// TODO: if the rename below fails, the next call will see legacyPath
	// still present and re-run the import. That's merely inefficient, not
	// corrupting: every row insert below is skip-on-conflict, so a repeat
	// import just re-skips everything it already imported.
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
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(id, parent_session_id, title, message_count, prompt_tokens, completion_tokens, cost, updated_at, created_at, summary_message_id, todos, project_path)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.id, s.parentSessionID, s.title, s.messageCount, s.promptTokens, s.completionTokens, s.cost, s.updatedAt, s.createdAt, s.summaryMessageID, s.todos, projectPath)
		if err != nil {
			slog.Warn("Skipping legacy session on import conflict", "session_id", s.id, "project_path", projectPath, "error", err)
			skippedSessions[s.id] = true
			counts.sessionsSkipped++
			continue
		}
		counts.sessionsImported++
	}

	if err := importLegacyMessages(ctx, legacyConn, tx, skippedSessions, &counts); err != nil {
		return counts, err
	}
	if err := importLegacyFiles(ctx, legacyConn, tx, skippedSessions, &counts); err != nil {
		return counts, err
	}
	if err := importLegacyReadFiles(ctx, legacyConn, tx, skippedSessions, &counts); err != nil {
		return counts, err
	}
	if err := importLegacyThreads(ctx, legacyConn, tx, projectPath, &counts); err != nil {
		return counts, err
	}

	if err := tx.Commit(); err != nil {
		return counts, fmt.Errorf("failed to commit import transaction: %w", err)
	}
	committed = true

	return counts, nil
}

func importLegacyMessages(ctx context.Context, legacyConn *sql.DB, tx *sql.Tx, skippedSessions map[string]bool, counts *legacyImportCounts) error {
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
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO messages
			(id, session_id, role, parts, model, created_at, updated_at, finished_at, provider, is_summary_message)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, sessionID, role, parts, model, createdAt, updatedAt, finishedAt, provider, isSummaryMessage)
		if err != nil {
			slog.Warn("Skipping legacy message on import conflict", "message_id", id, "error", err)
			counts.messagesSkipped++
			continue
		}
		counts.messagesImported++
	}
	return rows.Err()
}

func importLegacyFiles(ctx context.Context, legacyConn *sql.DB, tx *sql.Tx, skippedSessions map[string]bool, counts *legacyImportCounts) error {
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
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO files
			(id, session_id, path, content, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, sessionID, path, content, version, createdAt, updatedAt)
		if err != nil {
			slog.Warn("Skipping legacy file on import conflict", "file_id", id, "error", err)
			counts.filesSkipped++
			continue
		}
		counts.filesImported++
	}
	return rows.Err()
}

func importLegacyReadFiles(ctx context.Context, legacyConn *sql.DB, tx *sql.Tx, skippedSessions map[string]bool, counts *legacyImportCounts) error {
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
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO read_files (session_id, path, read_at) VALUES (?, ?, ?)`, sessionID, path, readAt)
		if err != nil {
			slog.Warn("Skipping legacy read_files row on import conflict", "session_id", sessionID, "path", path, "error", err)
			counts.readFilesSkipped++
			continue
		}
		counts.readFilesImported++
	}
	return rows.Err()
}

func importLegacyThreads(ctx context.Context, legacyConn *sql.DB, tx *sql.Tx, projectPath string, counts *legacyImportCounts) error {
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
		// project_path is stamped with the current project, overriding
		// whatever the legacy migration defaulted it to ('').
		_, err := tx.ExecContext(ctx, `INSERT INTO threads
			(id, name, project_path, goal, base_branch, branch, worktree_path, session_id, status, merge_policy, result_summary, error, created_at, updated_at, completed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, name, projectPath, goal, baseBranch, branch, worktreePath, sessionID, status, mergePolicy, resultSummary, errCol, createdAt, updatedAt, completedAt)
		if err != nil {
			slog.Warn("Skipping legacy thread on import conflict", "thread_id", id, "project_path", projectPath, "error", err)
			counts.threadsSkipped++
			continue
		}
		counts.threadsImported++
	}
	return rows.Err()
}
