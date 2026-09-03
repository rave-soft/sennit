package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/spf13/cobra"
)

// cmdContext returns cmd.Context(), falling back to context.Background().
// cmd.Context() is nil unless the command was dispatched through
// Execute(); RunE functions also run directly in tests, which never set
// one.
func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// initConfig resolves the working directory (via ResolveCwd) and loads
// config the way every read-only CLI command does. debug is taken as a
// parameter rather than read from the --debug flag internally because
// callers disagree on it: doctor/import/models pass the flag's value,
// while session/gc/stat force it off to keep those commands quiet
// regardless of --debug.
func initConfig(cmd *cobra.Command, debug bool) (cwd string, cfg *config.ConfigStore, err error) {
	cwd, err = ResolveCwd(cmd)
	if err != nil {
		return "", nil, err
	}
	dataDir, _ := cmd.Flags().GetString("data-dir")
	trustProject, _ := cmd.Flags().GetBool("trust-project")
	if trustProject {
		if err := config.Trust(cwd); err != nil {
			return "", nil, fmt.Errorf("failed to trust project: %w", err)
		}
	}
	cfg, err = configruntime.Load(cwd, dataDir, debug)
	if err != nil {
		return "", nil, err
	}
	// setupLocalWorkspace's own callers (interactive, `sennit run`) get a
	// file logger through PostConnect; every other command loads config
	// through here instead, and without this call their startup
	// diagnostics (e.g. config/migrate's deprecated-key warnings) stayed
	// buffered in earlyLogs until the process exited and vanished.
	setupProcessLogging(cmd, debug)
	return cwd, cfg, nil
}

// connectDB opens the shared pooled SQLite connection and its generated
// queries, the way every command that reads the database directly does.
// cleanup releases (not closes) the pooled connection: db.Connect pools by
// absolute path, and another caller in this process may hold the same
// shared global DB open, so closing it directly would corrupt the pool's
// refcount for them.
func connectDB(ctx context.Context) (q *db.Queries, conn *sql.DB, cleanup func(), err error) {
	conn, err = db.Connect(ctx, config.GlobalDBDir())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	return db.New(conn), conn, func() { _ = db.Release(config.GlobalDBDir()) }, nil
}

// emitJSON writes v to w as a single JSON document without escaping HTML
// characters (<, >, &) -- the repeated json.NewEncoder + SetEscapeHTML(false)
// + Encode triple every --json output path in this package needs.
func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// sessionByIDLister is the narrow dependency resolveSessionID needs: a
// direct-ID lookup plus a listing to fall back to for hash/hash-prefix
// resolution. sessionstore.Service already satisfies it; workspaceSessionLookup
// adapts workspace.Workspace's differently-named methods to it below.
type sessionByIDLister interface {
	Get(ctx context.Context, id string) (session.Session, error)
	List(ctx context.Context) ([]session.Session, error)
}

// workspaceSessionLookup adapts workspace.SessionStore's GetSession/
// ListSessions to sessionByIDLister so resolveSessionID can resolve a
// session ID against a Workspace the same way it does against a
// sessionstore.Service.
type workspaceSessionLookup struct{ ws workspace.SessionStore }

func (w workspaceSessionLookup) Get(ctx context.Context, id string) (session.Session, error) {
	return w.ws.GetSession(ctx, id)
}

func (w workspaceSessionLookup) List(ctx context.Context) ([]session.Session, error) {
	return w.ws.ListSessions(ctx)
}

// resolveSessionID resolves a session ID that can be a UUID, full hash, or
// hash prefix. Returns an error if the prefix is ambiguous (matches
// multiple sessions), listing the candidates like Git does for an
// ambiguous abbreviated commit hash.
func resolveSessionID(ctx context.Context, lookup sessionByIDLister, id string) (session.Session, error) {
	// Try direct UUID lookup first. Only a documented miss can fall back to
	// hash-prefix resolution; a cancelled request or storage failure must reach
	// the caller unchanged.
	if s, err := lookup.Get(ctx, id); err == nil {
		return s, nil
	} else if !errors.Is(err, session.ErrNotFound) {
		return session.Session{}, err
	}

	// List all sessions and check for hash matches.
	sessions, err := lookup.List(ctx)
	if err != nil {
		return session.Session{}, err
	}

	var matches []session.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	if len(matches) == 0 {
		return session.Session{}, fmt.Errorf("session not found: %s", id)
	}

	if len(matches) == 1 {
		return matches[0], nil
	}

	// Ambiguous - show matches like Git does.
	var sb strings.Builder
	fmt.Fprintf(&sb, "session ID '%s' is ambiguous. Matches:\n\n", id)
	for _, m := range matches {
		hash := session.HashID(m.ID)
		created := time.Unix(m.CreatedAt, 0).Format("2006-01-02")
		// Keep title on one line by replacing newlines with spaces, and truncate.
		title := strings.ReplaceAll(m.Title, "\n", " ")
		title = ansi.Truncate(title, 50, "…")
		fmt.Fprintf(&sb, "  %s... %q (created %s)\n", hash[:12], title, created) // ok: ascii — a hex session hash
	}
	sb.WriteString("\nUse more characters or the full hash")
	return session.Session{}, errors.New(sb.String())
}
