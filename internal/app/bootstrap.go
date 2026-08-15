package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	gitpkg "github.com/rave-soft/braid/internal/git"
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

	// InheritedAgents supplies user-defined agents from a parent workspace.
	// The child workspace's own definitions take precedence.
	InheritedAgents map[string]config.Agent

	// ConfineWrites keeps this workspace's file writes inside its own
	// working directory (see permission.Service.ConfinedDir). Set for a
	// thread, whose isolation in a git worktree is the whole point of it
	// and must not depend on a permission prompt nobody will see.
	ConfineWrites bool

	// WorkspaceLock enables a repository-scoped workspace lock. Git
	// workspaces lock their canonical common directory; non-git
	// workspaces lock their data directory.
	WorkspaceLock bool

	// GlobalSkillsMirror enables skills.WithGlobalMirror. Local mode
	// hosts a single workspace per process and sets this so the
	// package-level globals the TUI reads stay in sync; the backend
	// hosts multiple workspaces concurrently and leaves it off to avoid
	// last-writer-wins cross-talk between them.
	GlobalSkillsMirror bool

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

	newApp func(context.Context, *sql.DB, *config.ConfigStore, *skills.Manager) (*App, error)
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
// workspace's .braid data directory exists, acquire its workspace lock,
// connect its database,
// discover its skills, then construct the App. Callers differ only in
// the details captured by BootstrapOptions; see its field comments.
func Bootstrap(ctx context.Context, path string, opts BootstrapOptions) (*BootstrapResult, error) {
	cfg, err := config.Init(path, opts.DataDir, opts.Debug)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize config: %w", err)
	}

	cfg.Overrides().SkipPermissionRequests = opts.YOLO
	cfg.Overrides().EnabledChannels = opts.Channels
	if len(opts.InheritedAgents) > 0 {
		cfg.SetupAgentsWithInherited(opts.InheritedAgents)
	}

	// ensureDotBraidDir already wraps its own errors with context, so no
	// further wrapping here.
	if err := ensureDotBraidDir(cfg.Config().Options.DataDirectory); err != nil {
		return nil, err
	}

	var wsLock *db.WorkspaceLock
	if opts.WorkspaceLock {
		lockDir, err := workspaceLockDir(ctx, cfg.WorkingDir(), cfg.Config().Options.DataDirectory)
		if err != nil {
			return nil, err
		}
		wsLock, err = db.AcquireWorkspaceLock(lockDir)
		if err != nil {
			return nil, err
		}
		defer func() {
			if wsLock != nil {
				wsLock.Release()
			}
		}()
	}

	if opts.PostDataDir != nil {
		if err := opts.PostDataDir(cfg); err != nil {
			return nil, err
		}
	}

	globalDBDir := config.GlobalDBDir()
	conn, err := db.Connect(ctx, globalDBDir)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	// Connect returns a pooled reference. Every error below must release this
	// reference rather than closing the shared *sql.DB directly.
	dbConnected := true
	defer func() {
		if dbConnected {
			if err := db.Release(globalDBDir); err != nil {
				slog.Error("Failed to release database after bootstrap error", "error", err)
			}
		}
	}()

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

	newApp := opts.newApp
	if newApp == nil {
		newApp = New
	}
	appInstance, err := newApp(ctx, conn, cfg, skillsMgr)
	if err != nil {
		if opts.OnAppInitFailure != nil {
			opts.OnAppInitFailure(err)
		}
		return nil, fmt.Errorf("failed to create app workspace: %w", err)
	}
	if opts.ConfineWrites {
		appInstance.Permissions().ConfineToWorkingDir()
	}

	// Keep the workspace lock through all repo-dependent teardown. In
	// particular, it must outlive background shells and LSP clients.
	if err := appInstance.AddFinalCleanup(func(context.Context) error {
		wsLock.Release()
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to register workspace lock cleanup: %w", err)
	}
	wsLock = nil
	dbConnected = false // App owns this pooled reference through mainDBRelease.

	return &BootstrapResult{App: appInstance, Config: cfg, Skills: skillsMgr}, nil
}

func workspaceLockDir(ctx context.Context, workspaceDir, dataDir string) (string, error) {
	// A path that does not exist cannot be a repository. This preserves the
	// backend's ability to create a workspace at a new path without treating
	// git's chdir failure as a non-repository signal.
	if _, err := os.Stat(workspaceDir); errors.Is(err, os.ErrNotExist) {
		return dataDir, nil
	} else if err != nil {
		return "", fmt.Errorf("failed to inspect workspace for repository lock: %w", err)
	}

	commonDir, err := gitpkg.CommonDir(ctx, workspaceDir)
	if err == nil {
		return commonDir, nil
	}
	if errors.Is(err, gitpkg.ErrNotRepository) {
		return dataDir, nil
	}
	return "", fmt.Errorf("failed to resolve repository workspace lock: %w", err)
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
// skills.DiscoverFromConfig expects.
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
