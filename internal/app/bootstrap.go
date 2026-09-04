package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/db"
	gitpkg "github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/herdr"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/workspacelock"
)

// BootstrapOptions configures Bootstrap. Fields marked "top-level" or
// "spawned thread" below note where the two current callers (the CLI's
// single top-level workspace and a thread's spawned sub-workspace)
// diverge; everything else is shared verbatim.
type BootstrapOptions struct {
	// DataDir, Debug, YOLO and Channels feed config.Init and the
	// resulting store's overrides.
	DataDir      string
	Debug        bool
	YOLO         bool
	Channels     []string
	TrustProject bool

	// InheritedAgents supplies user-defined agents from a parent workspace.
	// The child workspace's own definitions take precedence.
	InheritedAgents map[string]config.Agent

	// PreferredModel, when set, overrides the model this workspace runs
	// on in memory only, before the agent coordinator is built. It is how
	// a thread inherits the model its parent workspace is actually
	// running: the parent's own selection may be an in-memory override
	// (a session's pinned model, `sennit run --model`) that was never
	// written to the config file this bootstrap reads, so without it a
	// thread would silently start on the file's model instead -- and
	// again on every restart.
	PreferredModel *config.SelectedModel

	// InheritedSkills supplies skills from a parent workspace, for a
	// child that cannot discover them itself — see
	// skills.DiscoveryConfig.InheritedSkills. The child workspace's own
	// definitions take precedence, and its DisabledSkills still apply.
	InheritedSkills []*skills.Skill

	// ConfineWrites keeps this workspace's file writes inside its own
	// working directory (see permission.Service.ConfinedDir). Set for a
	// thread, whose isolation in a git worktree is the whole point of it
	// and must not depend on a permission prompt nobody will see.
	ConfineWrites bool

	// WorkspaceLock enables a repository-scoped workspace lock. Git
	// workspaces lock their canonical common directory; non-git
	// workspaces lock their data directory.
	WorkspaceLock bool

	// PostDataDir, if set, runs after the .sennit data directory has
	// been created and before the DB connection is opened. The
	// top-level workspace uses this to register the project with the
	// projects package.
	PostDataDir func(cfg *config.ConfigStore) error

	// PostConnect, if set, runs after the DB connection is opened and
	// before skills discovery. The top-level workspace uses this to
	// point logging at the workspace's log file.
	PostConnect func(cfg *config.ConfigStore) error

	// OnAppInitFailure, if set, is called with New's raw error if it
	// fails. The top-level workspace uses this to log the failure; a
	// spawned thread's workspace reports it to its caller instead.
	OnAppInitFailure func(err error)
	HerdrClient      func() *herdr.Client

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
// workspace's .sennit data directory exists, acquire its workspace lock,
// connect its database,
// discover its skills, then construct the App. Callers differ only in
// the details captured by BootstrapOptions; see its field comments.
func Bootstrap(ctx context.Context, path string, opts BootstrapOptions) (*BootstrapResult, error) {
	if opts.TrustProject {
		if err := config.Trust(path); err != nil {
			return nil, fmt.Errorf("failed to trust project: %w", err)
		}
	}
	cfg, err := configruntime.Load(path, opts.DataDir, opts.Debug)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize config: %w", err)
	}

	cfg.SetSkipPermissionRequests(opts.YOLO)
	cfg.SetEnabledChannels(opts.Channels)
	if len(opts.InheritedAgents) > 0 {
		cfg.SetupAgentsWithInherited(opts.InheritedAgents)
	}
	// Before New: the agent coordinator is built from the config's model,
	// so overriding it here means the workspace starts on the inherited
	// model rather than switching onto it afterwards. An empty model is
	// "nothing inherited", not "clear the selection".
	if opts.PreferredModel != nil && opts.PreferredModel.Model != "" {
		cfg.OverridePreferredModel(*opts.PreferredModel)
	}

	// ensureDataDir already wraps its own errors with context, so no
	// further wrapping here.
	if err := ensureDataDir(cfg.Config().Options.DataDirectory); err != nil {
		return nil, err
	}

	var wsLock *workspacelock.Lock
	if opts.WorkspaceLock {
		lockDir, err := workspaceLockDir(ctx, cfg.WorkingDir(), cfg.Config().Options.DataDirectory)
		if err != nil {
			return nil, err
		}
		wsLock, err = workspacelock.Acquire(lockDir)
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

	if opts.PostConnect != nil {
		if err := opts.PostConnect(cfg); err != nil {
			return nil, err
		}
	}

	// Discover skills once per workspace, before New.
	discoveryCfg := SkillsDiscoveryConfig(cfg)
	discoveryCfg.InheritedSkills = opts.InheritedSkills
	allSkills, activeSkills, skillStates := skills.DiscoverFromConfig(discoveryCfg)
	skillOpts := []skills.ManagerOption{
		skills.WithResolvedPaths(discoveryCfg.ResolvePaths()),
		skills.WithWorkingDir(discoveryCfg.WorkingDir),
		skills.WithInheritedSkills(opts.InheritedSkills),
	}
	skillsMgr := skills.NewManager(allSkills, activeSkills, skillStates, skillOpts...)

	newApp := opts.newApp
	if newApp == nil {
		herdrClient := opts.HerdrClient
		if herdrClient == nil {
			herdrClient = herdr.Init
		}
		newApp = func(ctx context.Context, conn *sql.DB, store *config.ConfigStore, manager *skills.Manager) (*App, error) {
			return New(ctx, conn, store, manager, WithHerdrClient(herdrClient))
		}
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
	appInstance.workspaceLockEnforced = opts.WorkspaceLock && wsLock.Enforced()

	// Close out whatever a previous process was killed in the middle of,
	// now — before anything of this project's is dispatched, which is what
	// lets "unfinished" be read as "abandoned". Failure is logged inside
	// and not fatal: a session that opens with a stale spinner beats one
	// that does not open. See finalizeInterruptedTurns.
	//
	// Only under a lock that actually excludes a second process, though.
	// "Unfinished" reads as "abandoned" solely because nobody else can be
	// running turns against these sessions, and SENNIT_SKIP_DATADIR_LOCK
	// makes Acquire hand back a lock that excludes nothing (see
	// workspacelock.Lock.Enforced). Under it a second sennit would write
	// tool-result errors and a canceled finish into the first one's turn
	// while it is still streaming - repair for a crash, corruption for a
	// live run. Leaving a stale spinner in that case is the cheaper wrong
	// answer of the two.
	switch {
	case !appInstance.WorkspaceLockEnforced():
		slog.Warn("Skipping interrupted-turn cleanup: no enforced workspace lock, so another sennit may be running turns here",
			"component", "app", "project_path", cfg.WorkingDir())
	default:
		if err := finalizeInterruptedTurns(ctx, cfg.WorkingDir(), appInstance.Messages()); err != nil {
			slog.Error("Failed to close out interrupted turns from a previous run",
				"component", "app", "project_path", cfg.WorkingDir(), "error", err)
		}
	}

	// Keep the workspace lock through all repo-dependent teardown. In
	// particular, it must outlive background shells and LSP clients.
	//
	// lockToRelease is captured into its own local rather than read from
	// wsLock directly: the closure below runs much later, during
	// Shutdown, and closures capture variables by reference — reading
	// wsLock there would see whatever wsLock is set to at that later
	// time, not what it held now. The very next line sets wsLock = nil
	// (to disarm the early-return defer above once ownership has moved
	// here), which used to leave this closure releasing a nil
	// *workspacelock.Lock: Release is a documented no-op on nil, so the OS
	// lock was silently never released.
	lockToRelease := wsLock
	if err := appInstance.AddFinalCleanup(func(context.Context) error {
		lockToRelease.Release()
		return nil
	}); err != nil {
		// appInstance is fully built at this point: New already wired
		// mainDBRelease unconditionally on its own success, so ownership of
		// the pooled DB reference has already moved from this function's
		// dbConnected defer to the App, unlike New's own late-failure path
		// (app.go's InitCoderAgent branch), which clears mainDBRelease
		// before calling Shutdown because ownership there never left the
		// caller. Disarm dbConnected here so that defer does not also
		// release it — Shutdown below is what actually releases it now —
		// and then tear the App down: MCP init, the config/skills watchers
		// and the herdr bridge are all live goroutines and this error
		// leaves the caller with no *App to shut down itself. (This branch
		// fires only once Shutdown has already started elsewhere, in which
		// case this call just joins that teardown instead of starting a
		// second one.)
		dbConnected = false
		appInstance.Shutdown()
		return nil, fmt.Errorf("failed to register workspace lock cleanup: %w", err)
	}
	wsLock = nil
	dbConnected = false // App owns this pooled reference through mainDBRelease.

	return &BootstrapResult{App: appInstance, Config: cfg, Skills: skillsMgr}, nil
}

func workspaceLockDir(ctx context.Context, workspaceDir, dataDir string) (string, error) {
	// A path that does not exist cannot be a repository. This preserves
	// the ability to create a workspace at a new path without treating
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

// ensureDataDir creates the workspace's .sennit data directory and,
// the first time, a .gitignore inside it that excludes the whole thing
// from the workspace's own git repo.
func ensureDataDir(dir string) error {
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
