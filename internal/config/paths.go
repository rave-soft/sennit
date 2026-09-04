package config

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/home"
)

// lookupConfigs searches config files starting at cwd and walking up
// through the current project. The upward walk stops at the git
// working tree root when one can be detected, otherwise at cwd itself,
// so an unrelated sennit.json placed above the project is never picked
// up. Global user-level config locations are always included
// regardless of the boundary.
func lookupConfigs(cwd string) []string {
	// Prepend global user config and machine-owned data JSON. Only the user
	// config directory contributes a sennitrc; the data directory is writable
	// machine state and must never be executed as Bash. Missing files are
	// skipped when loaded.
	configPaths := globalConfigPaths()

	// Ordered high-to-low priority within a directory. LookupBounded returns
	// matches in this order, and the later reverse + merge make the earliest
	// listed name win on conflict. So: the .sennit/ subdirectory variants beat
	// their root-level counterparts, .sennitrc beats sennitrc, both beat the
	// JSON configs, and .sennit.json beats sennit.json.
	//
	// The .sennit/ variants are looked up as literal names — ".sennit" here is
	// not options.data_directory (which is configurable and resolved
	// separately, see workspacePath in Load/reloadFromDisk); it is the
	// project's canonical config subdirectory, checked at every directory in
	// the upward walk just like the other names. Both this name and
	// defaultDataDirectory come from brand.DataDir, which is what keeps the
	// two from drifting apart.
	configNames := []string{
		filepath.Join(defaultDataDirectory, brand.ShellConfigFile),
		brand.HiddenShellConfigFile,
		brand.ShellConfigFile,
		filepath.Join(defaultDataDirectory, brand.JSONConfigFile),
		brand.HiddenJSONConfigFile,
		brand.JSONConfigFile,
	}

	foundConfigs, err := fsext.LookupBounded(cwd, projectBoundary(cwd), configNames...)
	if err != nil {
		// returns at least default configs
		return configPaths
	}

	// reverse order so last config has more priority
	slices.Reverse(foundConfigs)

	return append(configPaths, foundConfigs...)
}

// GlobalConfig returns the global configuration file path for the application.
func GlobalConfig() string {
	if globalOverride := os.Getenv(brand.EnvPrefix + "GLOBAL_CONFIG"); globalOverride != "" {
		return filepath.Join(globalOverride, fmt.Sprintf("%s.json", appName))
	}
	return filepath.Join(home.Config(), appName, fmt.Sprintf("%s.json", appName))
}

// GlobalDBDir returns the directory holding the single SQLite database
// shared by every project, ~/.config/sennit by default (or
// SENNIT_GLOBAL_CONFIG's directory when set). Every workspace connects to
// the same sennit.db; rows are scoped by project_path.
func GlobalDBDir() string {
	return filepath.Dir(GlobalConfig())
}

// GlobalLogDir returns the directory holding every process's log,
// ~/.config/sennit/logs by default (alongside the shared database — see
// GlobalDBDir).
func GlobalLogDir() string {
	return filepath.Join(GlobalDBDir(), "logs")
}

// GlobalLogFile returns the path to *this process's own* log file,
// ~/.config/sennit/logs/sennit-<pid>.log by default.
//
// A single shared log file interleaves entries from every sennit running on
// the machine — and several usually are, since a sennit works in one
// session while a person works on more than one thing — so unrelated
// top-level sessions can read as one process dispatching concurrent
// delegations to itself.
//
// A file per process makes that question unaskable. Both the writer
// (log.Setup) and the in-process readers (the sennit_logs and
// agent_trace tools) resolve through here, so they agree on which file
// is "the log" without being told; `sennit logs`, which runs in a
// process of its own and means somebody else's log, resolves through
// LatestGlobalLogFile instead.
func GlobalLogFile() string {
	return filepath.Join(GlobalLogDir(), fmt.Sprintf("%s-%d.log", brand.Slug, os.Getpid()))
}

// isRunLogName reports whether name is a log written by a running
// sennit, as opposed to a panic dump (RecoverPanic writes
// sennit-panic-<name>-<time>.log into this same directory). A panic dump
// is by definition the newest file the moment it lands, so without this
// `sennit logs` would answer a request for the running sennit's log with
// a single stack trace. internal/log's own sweep keeps the same
// distinction, for the other reason: a panic dump is the last thing
// anyone would want tidied away.
func isRunLogName(name string) bool {
	return strings.HasSuffix(name, ".log") && !strings.Contains(name, "-panic-")
}

// LatestGlobalLogFile returns the most recently written log in
// GlobalLogDir — for a reader that means "the sennit that is running",
// not its own process. That is `sennit logs`, which is a separate
// process every time and would otherwise resolve to a log file it just
// created and never wrote to.
//
// The legacy shared sennit.log is included in the search rather than
// special-cased: on an install that predates per-process logs it is the
// only file there, and it is still the newest until something writes a
// new one. When the directory is empty or unreadable, this returns this
// process's own path, so a caller's "no logs yet" message names a
// plausible file.
func LatestGlobalLogFile() string {
	dir := GlobalLogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return GlobalLogFile()
	}
	// Skip this process's own file by name. The exclusion used to hold by
	// accident: a reader never opened a log of its own, so it could not
	// win the newest-mtime race. It does now — every command that loads
	// config installs a file logger, and `sennit logs` loads config —
	// which made the reader answer with the log it had just written its
	// own startup lines into.
	own := filepath.Base(GlobalLogFile())
	var newest string
	var newestAt int64
	for _, entry := range entries {
		if entry.IsDir() || !isRunLogName(entry.Name()) || entry.Name() == own {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if at := info.ModTime().UnixNano(); at > newestAt {
			newest, newestAt = filepath.Join(dir, entry.Name()), at
		}
	}
	if newest == "" {
		return GlobalLogFile()
	}
	return newest
}

// GlobalAccountsFile returns the path to the provider account store,
// ~/.config/sennit/accounts.json by default (or alongside SENNIT_GLOBAL_
// CONFIG's directory when set). It lives next to GlobalConfig rather than
// under a workspace: providers are global-only in this project (see
// globalonly.go), and an account belongs to a provider, not a project.
//
// It is a file of its own rather than a section of sennit.json because
// sennit.json is meant to be read and hand-edited by users, and OAuth
// tokens for several accounts per provider — plus usage snapshots that
// churn far more often than the rest of the config — would bloat it into
// something nobody wants to open. See internal/providers/accounts' package
// doc for the account model this file holds.
func GlobalAccountsFile() string {
	return filepath.Join(GlobalDBDir(), "accounts.json")
}

// shellConfigSibling returns the sennitrc path that sits alongside a given
// sennit.json path (same directory). Used so global config locations pick up a
// shell config, not just JSON.
func shellConfigSibling(jsonPath string) string {
	return filepath.Join(filepath.Dir(jsonPath), appName+"rc")
}

// isShellConfig reports whether a config path is a shell config (sennitrc or
// the hidden .sennitrc), as opposed to a JSON config.
func isShellConfig(path string) bool {
	base := filepath.Base(path)
	return base == appName+"rc" || base == "."+appName+"rc"
}

// ProjectConfigs returns list of current project configs paths.
func ProjectConfigs(cwd string) []string {
	return lookupConfigs(cwd)
}

// GlobalConfigData returns the path to the main data directory for the application.
// this config is used when the app overrides configurations instead of updating the global config.
func GlobalConfigData() string {
	if dataOverride := os.Getenv(brand.EnvPrefix + "GLOBAL_DATA"); dataOverride != "" {
		return filepath.Join(dataOverride, fmt.Sprintf("%s.json", appName))
	}
	if xdgDataHome := os.Getenv("XDG_DATA_HOME"); xdgDataHome != "" {
		return filepath.Join(xdgDataHome, appName, fmt.Sprintf("%s.json", appName))
	}

	// return the path to the main data directory
	// for windows, it should be in `%LOCALAPPDATA%/sennit/`
	// for linux and macOS, it should be in `$HOME/.local/share/sennit/`
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(localAppData, appName, fmt.Sprintf("%s.json", appName))
	}

	return filepath.Join(home.Dir(), ".local", "share", appName, fmt.Sprintf("%s.json", appName))
}

// isInsideWorktree reports whether dir is inside a git working tree
// (regular or linked worktree), as opposed to a bare repository or a
// plain non-git directory. It answers for dir, not the process cwd, so
// callers must pass the workspace directory they actually care about.
func isInsideWorktree(dir string) bool {
	return worktreeRoot(dir) != ""
}

// worktreeRoot returns the absolute path of the git working tree root for
// dir, or the empty string if dir is not inside a working tree (bare
// repositories, missing git binary, plain directories, or any other
// failure mode). Linked worktrees and submodules each report their own
// top-level, which is what callers want when bounding lookups.
// worktreeRootCache memoizes positive git worktree root lookups per
// directory, so we avoid re-shelling out to "git rev-parse" on every config
// reload. Once dir resolves to a worktree root, that root is stable for the
// life of the process and safe to cache. A negative result ("" — dir is not
// in a worktree) is never cached: `git init` can turn a plain directory into
// one mid-session, and caching "" would leave that dir stuck outside its new
// worktree boundary until the process restarts.
//
// worktreeRoot is on a hot, polled path — WatchForExternalChanges (watch.go)
// calls lookupConfigs, and therefore this, on every tick — so the uncached
// negative case must not shell out. findGitEntry answers it with a plain
// upward stat walk instead; only a directory that actually has a .git entry
// pays for a git subprocess.
var worktreeRootCache sync.Map // map[string]string

func worktreeRoot(dir string) string {
	if cached, ok := worktreeRootCache.Load(dir); ok {
		return cached.(string)
	}
	if !findGitEntry(dir) {
		return ""
	}
	root := computeWorktreeRoot(dir)
	if root != "" {
		worktreeRootCache.Store(dir, root)
	}
	return root
}

// findGitEntry reports whether dir or one of its ancestors has a .git entry
// (a directory for a normal clone, a file for a linked worktree or
// submodule). It walks upward with plain stats, stopping at $HOME or the
// filesystem root, whichever comes first — the same bound lookupClosest
// uses in fsext/lookup.go — so an unrelated .git far above the user's home
// directory is never treated as project state, and a deeply nested dir
// never turns into an unbounded walk. This is the cheap pre-check that lets
// worktreeRoot's negative case go uncached without shelling out to git on
// every poll.
func findGitEntry(dir string) bool {
	cwd, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	homeDir := fsext.Canonical(home.Dir())

	for {
		if _, err := os.Lstat(filepath.Join(cwd, ".git")); err == nil {
			return true
		}
		if fsext.Canonical(cwd) == homeDir {
			return false
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return false
		}
		cwd = parent
	}
}

func computeWorktreeRoot(dir string) string {
	cmd := exec.CommandContext(
		context.Background(),
		"git", "rev-parse", "--show-toplevel",
	)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	return abs
}

// projectBoundary returns the directory at which an upward configuration
// search rooted at dir should stop. It is the git working tree root when
// one can be detected, otherwise dir itself. Returning dir as a
// fallback keeps Sennit from silently adopting state files placed above
// the current project.
func projectBoundary(dir string) string {
	if root := worktreeRoot(dir); root != "" {
		return root
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// GlobalSkillsDirs returns the default directories for Agent Skills.
// Skills in these directories are auto-discovered and their files can be read
// without permission prompts.
//
// Only Sennit's own catalog is scanned here. Skills authored for other tools
// (Claude Code, opencode, ...) are not auto-discovered — see `sennit import`,
// which copies them into .sennit/skills with validation instead of trusting a
// foreign directory implicitly.
func GlobalSkillsDirs() []string {
	if skillsOverride := os.Getenv(brand.EnvPrefix + "SKILLS_DIR"); skillsOverride != "" {
		return []string{skillsOverride}
	}

	paths := []string{
		filepath.Join(home.Config(), appName, "skills"),
	}

	// On Windows, also load from app data on top of `$HOME/.config/sennit`.
	// This is here mostly for backwards compatibility.
	if runtime.GOOS == "windows" {
		appData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		paths = append(paths, filepath.Join(appData, appName, "skills"))
	}

	return paths
}

// projectSkillSubdirs lists the conventional subdirectories where
// project-level skills are discovered. Shared across working-dir and
// git-root lookups to prevent drift when a new convention is added.
//
// Only .sennit/skills is scanned: skills written for other tools are brought
// in explicitly via `sennit import`, not auto-discovered from their native
// directories.
var projectSkillSubdirs = []string{
	brand.DataDir + "/skills",
}

// ProjectSkillsDir returns the default project directories for which Sennit
// will look for skills. In addition to the working directory, it also
// checks the git working tree root so that monorepo-level skills are
// discovered when the user is inside a subdirectory.
//
// The git-root paths come first and the working-directory paths come last:
// skills.DiscoverWithStates walks paths in this order and skills.Deduplicate
// keeps the last occurrence of a name, so the last path here is the one
// that wins. That makes local skills take precedence over monorepo-level
// ones of the same name.
func ProjectSkillsDir(workingDir string) []string {
	dirs := make([]string, 0, len(projectSkillSubdirs)*2)

	// When the working directory is inside a git repository, also look at
	// the repository root so monorepo-level .agents/skills are found. This
	// goes first so the working-directory entries appended below win ties.
	if root := worktreeRoot(workingDir); root != "" && root != workingDir {
		for _, sub := range projectSkillSubdirs {
			dirs = append(dirs, filepath.Join(root, sub))
		}
	}

	for _, sub := range projectSkillSubdirs {
		dirs = append(dirs, filepath.Join(workingDir, sub))
	}

	return dirs
}
