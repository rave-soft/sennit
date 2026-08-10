package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/skills"
)

// BootstrapOptions configures Bootstrap. Fields marked "backend" or
// "local" below note where the two current callers (the multi-workspace
// backend and the single-workspace CLI) diverge; everything else is
// shared verbatim.
type BootstrapOptions struct {
	// DataDir, Debug, YOLO and Channels feed config.Init and the
	// resulting store's overrides.
	DataDir  string
	Debug    bool
	YOLO     bool
	Channels []string

	// WorkspaceLock enables db.AcquireWorkspaceLock on the workspace's
	// .braid data directory. The backend sets this (it hosts multiple
	// concurrent workspaces and must guard each one's data directory
	// against a second braid process racing it); local mode leaves it
	// off.
	WorkspaceLock bool

	// GlobalSkillsMirror enables skills.WithGlobalMirror. Local mode
	// hosts a single workspace per process and sets this so the
	// package-level globals the TUI reads stay in sync; the backend
	// hosts multiple workspaces concurrently and leaves it off to avoid
	// last-writer-wins cross-talk between them.
	GlobalSkillsMirror bool

	// CloseDBOnAppFailure closes the DB connection if app.New fails.
	// Local mode does this (it owns the only reference to the
	// connection); the backend does not.
	CloseDBOnAppFailure bool

	// PostDataDir, if set, runs after the .braid data directory has
	// been created and before the DB connection is opened. Local mode
	// uses this to register the project with the projects package.
	PostDataDir func(cfg *config.ConfigStore) error

	// PostConnect, if set, runs after the DB connection is opened and
	// before skills discovery. Local mode uses this to point logging at
	// the workspace's log file.
	PostConnect func(cfg *config.ConfigStore) error

	// OnAppInitFailure, if set, is called with New's raw error if it
	// fails. Local mode uses this to log the failure; the backend
	// reports it to its caller instead.
	OnAppInitFailure func(err error)
}

// BootstrapResult holds the pieces Bootstrap assembles, for callers that
// need to wire them into their own workspace types.
type BootstrapResult struct {
	App    *App
	Config *config.ConfigStore
	Skills *skills.Manager
}

// Bootstrap runs the workspace bootstrap sequence shared by every place
// that starts an in-process app.App: initialize config, ensure the
// workspace's .braid data directory exists, connect its database,
// discover its skills, then construct the App. Callers differ only in
// the details captured by BootstrapOptions; see its field comments.
func Bootstrap(ctx context.Context, path string, opts BootstrapOptions) (*BootstrapResult, error) {
	cfg, err := config.Init(path, opts.DataDir, opts.Debug)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize config: %w", err)
	}

	cfg.Overrides().SkipPermissionRequests = opts.YOLO
	cfg.Overrides().EnabledChannels = opts.Channels

	// ensureDotBraidDir already wraps its own errors with context, so no
	// further wrapping here.
	if err := ensureDotBraidDir(cfg.Config().Options.DataDirectory); err != nil {
		return nil, err
	}

	if opts.PostDataDir != nil {
		if err := opts.PostDataDir(cfg); err != nil {
			return nil, err
		}
	}

	// The workspace lock guards a project's .braid directory against a
	// second braid process racing it; it is unrelated to which database
	// file Connect opens (that's now always the shared global one), so
	// it is acquired independently, before Connect.
	var wsLock *db.WorkspaceLock
	if opts.WorkspaceLock {
		wsLock, err = db.AcquireWorkspaceLock(cfg.Config().Options.DataDirectory)
		if err != nil {
			return nil, err
		}
	}

	conn, err := db.Connect(ctx, config.GlobalDBDir())
	if err != nil {
		wsLock.Release()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// One-time, best-effort import of this project's legacy per-project
	// database (from before every project moved to one shared database).
	// Never blocks Bootstrap: a failure here just means the project's old
	// history isn't visible yet, not that the workspace can't start.
	if err := db.ImportLegacyProjectDB(ctx, cfg.Config().Options.DataDirectory, cfg.WorkingDir(), conn); err != nil {
		slog.Warn("Failed to import legacy project database", "error", err)
	}

	if opts.PostConnect != nil {
		if err := opts.PostConnect(cfg); err != nil {
			return nil, err
		}
	}

	// Discover skills once per workspace, before New.
	discoveryCfg := SkillsDiscoveryConfig(cfg)
	allSkills, activeSkills, skillStates := skills.DiscoverFromConfig(discoveryCfg)
	skillOpts := []skills.ManagerOption{
		skills.WithResolvedPaths(discoveryCfg.ResolvePaths()),
		skills.WithWorkingDir(discoveryCfg.WorkingDir),
	}
	if opts.GlobalSkillsMirror {
		skillOpts = append([]skills.ManagerOption{skills.WithGlobalMirror()}, skillOpts...)
	}
	skillsMgr := skills.NewManager(allSkills, activeSkills, skillStates, skillOpts...)

	appInstance, err := New(ctx, conn, cfg, skillsMgr)
	if err != nil {
		wsLock.Release()
		if opts.CloseDBOnAppFailure {
			_ = conn.Close()
		}
		if opts.OnAppInitFailure != nil {
			opts.OnAppInitFailure(err)
		}
		return nil, fmt.Errorf("failed to create app workspace: %w", err)
	}

	// Release the workspace lock (if any) on the same schedule as the
	// rest of the workspace's resources.
	appInstance.AddCleanup(func(context.Context) error {
		wsLock.Release()
		return nil
	})

	return &BootstrapResult{App: appInstance, Config: cfg, Skills: skillsMgr}, nil
}

// ensureDotBraidDir creates the workspace's .braid data directory and,
// the first time, a .gitignore inside it that excludes the whole thing
// from the workspace's own git repo.
func ensureDotBraidDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create data directory: %q %w", dir, err)
	}

	gitIgnorePath := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gitIgnorePath); os.IsNotExist(err) {
		if err := os.WriteFile(gitIgnorePath, []byte("*\n"), 0o644); err != nil {
			return fmt.Errorf("failed to create .gitignore file: %q %w", gitIgnorePath, err)
		}
	}

	return nil
}

// SkillsDiscoveryConfig adapts a *config.ConfigStore to the inputs
// skills.DiscoverFromConfig expects. Exported so internal/backend can
// build the same DiscoveryConfig for its skills-directory watcher
// (see backend.createWorkspace) without duplicating the field mapping.
func SkillsDiscoveryConfig(cfg *config.ConfigStore) skills.DiscoveryConfig {
	opts := cfg.Config().Options
	var paths, disabled []string
	if opts != nil {
		paths = opts.SkillsPaths
		disabled = opts.DisabledSkills
	}
	var resolver func(string) (string, error)
	if r := cfg.Resolver(); r != nil {
		resolver = r.ResolveValue
	}
	return skills.DiscoveryConfig{
		SkillsPaths:    paths,
		DisabledSkills: disabled,
		WorkingDir:     cfg.WorkingDir(),
		Resolver:       resolver,
	}
}
