package prompt

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
)

// ConfigProvider is the slice of *config.ConfigStore prompt building needs:
// the config snapshot, the variable resolver used to expand context-file
// paths, and the working directory those paths are relative to. Declaring it
// here rather than accepting the concrete *config.ConfigStore keeps prompt
// building a pure read of config: it cannot write config fields, activate an
// account, persist credentials or reload from disk. Building a system prompt
// happens on every turn, and a step on that path that could mutate the store
// would make the prompt an input to the next prompt.
type ConfigProvider interface {
	Config() *config.Config
	Resolver() config.VariableResolver
	WorkingDir() string
}

// The port is a narrowing of the store's contract, never a divergent one:
// this fails to compile the moment the two disagree.
var _ ConfigProvider = (*config.ConfigStore)(nil)

// SkillsProvider is an optional capability of a ConfigProvider: a caller
// that already holds the coordinator's computed active-skill list (see
// skills.Manager.ActiveSkills) can hand it to promptData instead of
// letting it rediscover skills from disk. It is a separate, narrower
// interface rather than a new ConfigProvider method so a ConfigProvider
// with no such list (nothing wired to a *skills.Manager) keeps compiling
// unchanged; promptData falls back to disk discovery via a type
// assertion when store does not implement it.
type SkillsProvider interface {
	ActiveSkills() []*skills.Skill
}

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name       string
	template   string
	now        func() time.Time
	platform   string
	workingDir string
}

type PromptDat struct {
	Provider           string
	Model              string
	Config             config.Config
	WorkingDir         string
	IsGitRepo          bool
	Platform           string
	Date               string
	GitStatus          string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string
	SkillsURIScheme    string
	// HasLSPTools mirrors the exact gate buildTools uses to register the
	// lsp_* tools (newAgentConfig: len(cfg.LSP) > 0 ||
	// AutoLSPEnabled()) - see coder.md.tpl's <editing_files> and <lsp>
	// blocks, which used to each spell out their own, different
	// condition (one omitted the tools entirely, the other tested only
	// len(.Config.LSP)) and could disagree with the real tool set and
	// with each other.
	HasLSPTools bool
}

type ContextFile struct {
	Path    string
	Content string
}

type Option func(*Prompt)

func WithTimeFunc(fn func() time.Time) Option {
	return func(p *Prompt) {
		p.now = fn
	}
}

func WithPlatform(platform string) Option {
	return func(p *Prompt) {
		p.platform = platform
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(p *Prompt) {
		p.workingDir = workingDir
	}
}

func NewPrompt(name, promptTemplate string, opts ...Option) (*Prompt, error) {
	p := &Prompt{
		name:     name,
		template: promptTemplate,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Prompt) Build(ctx context.Context, provider, model string, store ConfigProvider) (string, error) {
	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var sb strings.Builder
	d := p.promptData(ctx, provider, model, store)
	if err := t.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return sb.String(), nil
}

func processFile(filePath string) *ContextFile {
	content, err := os.ReadFile(filePath)
	if err != nil {
		// The path already passed os.Stat in the caller, so this is not
		// "file doesn't exist" — it is something like a permissions
		// problem. Silently dropping it here used to mean a project's
		// AGENTS.md could vanish from the prompt without a trace, unlike
		// processContextPath's own error path just above it.
		slog.Warn("Failed to read context file", "path", filePath, "error", err)
		return nil
	}
	return &ContextFile{
		Path:    filePath,
		Content: string(content),
	}
}

func processContextPath(p string, store ConfigProvider) []ContextFile {
	var contexts []ContextFile
	fullPath := filepathext.SmartJoin(store.WorkingDir(), p)
	info, err := os.Stat(fullPath)
	if err != nil {
		return contexts
	}
	if info.IsDir() {
		if err := filepath.WalkDir(fullPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				if result := processFile(path); result != nil {
					contexts = append(contexts, *result)
				}
			}
			return nil
		}); err != nil {
			slog.Warn("Failed to walk context path", "path", fullPath, "error", err)
		}
	} else {
		result := processFile(fullPath)
		if result != nil {
			contexts = append(contexts, *result)
		}
	}
	return contexts
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, store ConfigProvider) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of
// paths, in the order paths lists them - so the same working directory
// always produces the same system-prompt text (see promptData), which
// matters for provider prompt caching on the cached prefix.
//
// Each path is stat'd before it is considered for dedup, and only two
// paths that resolve to the SAME on-disk file (os.SameFile - same device
// and inode, so this works for a symlink or a hardlink too, not just an
// identical string) collapse into one entry. Deduplicating on the
// case-folded path string instead used to mean that on a case-sensitive
// filesystem a path that doesn't exist (e.g. "AGENTS.md" when only
// "agents.md" is present) still claimed the dedup key and silently
// prevented the real file from ever being stat'd - config.go deliberately
// lists AGENTS.md, agents.md, and Agents.md as separate candidates
// precisely so a project using any one casing is found, so a Linux project
// with only agents.md must not be treated as having no context file at
// all.
func loadContextFiles(paths []string, store ConfigProvider) []ContextFile {
	var result []ContextFile
	var seen []os.FileInfo
	for _, pth := range paths {
		expanded := expandPath(pth, store)
		fullPath := filepathext.SmartJoin(store.WorkingDir(), expanded)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(seen, func(s os.FileInfo) bool { return os.SameFile(s, info) }) {
			continue
		}
		seen = append(seen, info)
		result = append(result, processContextPath(expanded, store)...)
	}
	return result
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store ConfigProvider) PromptDat {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	cfg := store.Config()
	contextFiles := loadContextFiles(cfg.Options.ContextPaths, store)
	globalContextFiles := loadContextFiles(cfg.Options.GlobalContextPaths, store)

	// Discover and load skills metadata.
	var availSkillXML string

	if sp, ok := store.(SkillsProvider); ok {
		// The coordinator has already discovered, deduplicated, filtered
		// and - critically - folded in InheritedSkills: a thread spawned
		// into its own git worktree has no .sennit/skills of its own, so
		// re-discovering from cfg.Options.SkillsPaths here would silently
		// drop every project skill the parent workspace activated, even
		// though the same thread's `read` tool and `sennit_info` still
		// report them as available (see skills.Manager.ActiveSkills).
		// Using that list verbatim also retires the second disagreement
		// this rediscovery caused: a relative skills_paths entry resolved
		// against store.WorkingDir() below, but DiscoveryConfig.ResolvePaths
		// (skills/manager.go) resolves the very same entry differently.
		if activeSkills := sp.ActiveSkills(); len(activeSkills) > 0 {
			availSkillXML = skills.ToPromptXML(activeSkills)
		}
	} else {
		// No coordinator-backed skill list available (e.g. the
		// initialize prompt, or a bare ConfigProvider in a test) - fall
		// back to discovering from disk, same as before SkillsProvider
		// existed.

		// Start with builtin skills.
		allSkills := skills.DiscoverBuiltin()
		builtinNames := make(map[string]bool, len(allSkills))
		for _, s := range allSkills {
			builtinNames[s.Name] = true
		}

		// Discover user skills from configured paths.
		if len(cfg.Options.SkillsPaths) > 0 {
			expandedPaths := make([]string, 0, len(cfg.Options.SkillsPaths))
			for _, pth := range cfg.Options.SkillsPaths {
				// Resolve against the workspace, not the process's cwd, the
				// same way processContextPath does for context files.
				// SmartJoin leaves an absolute path untouched, so only a
				// relative entry is anchored. Without this a relative
				// skills_paths entry means something different depending on
				// where sennit happened to be launched from.
				expandedPaths = append(expandedPaths, filepathext.SmartJoin(
					store.WorkingDir(), expandPath(pth, store)))
			}
			for _, userSkill := range skills.Discover(expandedPaths) {
				if builtinNames[userSkill.Name] {
					slog.Warn("User skill overrides builtin skill", "name", userSkill.Name)
				}
				allSkills = append(allSkills, userSkill)
			}
		}

		// Deduplicate: user skills override builtins with the same name.
		allSkills = skills.Deduplicate(allSkills)

		// Filter out disabled skills.
		allSkills = skills.Filter(allSkills, cfg.Options.DisabledSkills)

		if len(allSkills) > 0 {
			availSkillXML = skills.ToPromptXML(allSkills)
		}
	}

	isGit := isGitRepo(ctx, store.WorkingDir())
	data := PromptDat{
		Provider:        provider,
		Model:           model,
		Config:          *cfg,
		WorkingDir:      filepath.ToSlash(workingDir),
		IsGitRepo:       isGit,
		Platform:        platform,
		Date:            p.now().Format("1/2/2006"),
		AvailSkillXML:   availSkillXML,
		SkillsURIScheme: brand.SkillsURIScheme,
		// Same formula as newAgentConfig's hasLSP/autoLSP (internal/agent/
		// agent_config.go): a configured LSP or an auto_lsp setting that
		// hasn't been explicitly turned off. Duplicated rather than
		// imported because internal/agent already imports this package,
		// so the reverse import would cycle; both sides read the same
		// two config fields and must be kept in step.
		HasLSPTools: len(cfg.LSP) > 0 || cfg.Options.AutoLSP == nil || *cfg.Options.AutoLSP,
	}
	if isGit {
		data.GitStatus = getGitStatus(ctx, store.WorkingDir())
	}

	data.ContextFiles = contextFiles
	data.GlobalContextFiles = globalContextFiles
	return data
}

// isGitRepo reports whether dir is inside a git working tree. It shells
// out to `git rev-parse --is-inside-work-tree` rather than stat'ing dir
// for a ".git" entry: the working directory is a session's cwd (or
// --cwd), not necessarily the repo root, and a plain stat says "no" for
// any subdirectory of a repo even though every git tool the prompt
// recommends works fine there and finds the same repo by walking up. A
// git failure that is not "not a repository" (missing binary, permission
// problem) is treated the same as "not a repo" - the safe default that
// simply omits the git-status block, matching getGitStatusSummary's own
// "could not be determined" rather than asserting either state.
func isGitRepo(ctx context.Context, dir string) bool {
	inRepo, err := git.IsRepo(ctx, dir)
	if err != nil {
		slog.Warn("Could not determine whether the working directory is a git repository", "dir", dir, "error", err)
		return false
	}
	return inRepo
}

func getGitStatus(ctx context.Context, dir string) string {
	sh := shell.NewShell(&shell.Options{
		WorkingDir: dir,
	})
	branch := getGitBranch(ctx, sh)
	status := getGitStatusSummary(ctx, sh)
	commits := getGitRecentCommits(ctx, sh)
	return branch + status + commits
}

// getGitBranch is best-effort: git failures are silently ignored and simply
// omit the branch line from the prompt.
func getGitBranch(ctx context.Context, sh *shell.Shell) string {
	out, _, err := sh.Exec(ctx, "git branch --show-current 2>/dev/null")
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	return fmt.Sprintf("Current branch: %s\n", out)
}

// getGitStatusSummary reports the working tree's status, or says plainly
// that it couldn't be determined rather than asserting "clean" by default.
//
// The truncation to 20 lines used to be a `| head -20` in the command
// itself, which silently swallowed the failure this function exists to
// report: piping into `head` replaces `git status`'s own exit status with
// `head`'s, which is always 0, so a git failure (a corrupt or
// partially-created repo, a permissions problem, a missing git binary —
// anything that gets past isGitRepo's bare `.git`-directory stat) produced
// an empty out with a nil err, indistinguishable from a genuinely clean
// tree. Truncating in Go instead lets `err` stay git's own, so a real
// failure is told apart from an empty, successful status.
func getGitStatusSummary(ctx context.Context, sh *shell.Shell) string {
	out, _, err := sh.Exec(ctx, "git status --short 2>/dev/null")
	if err != nil {
		return "Status: could not be determined (git failed)\n"
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "Status: clean\n"
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 20 {
		lines = lines[:20]
	}
	return fmt.Sprintf("Status:\n%s\n", strings.Join(lines, "\n"))
}

// getGitRecentCommits is best-effort: git failures are silently ignored and
// simply omit the commits line from the prompt.
func getGitRecentCommits(ctx context.Context, sh *shell.Shell) string {
	out, _, err := sh.Exec(ctx, "git log --oneline -n 3 2>/dev/null")
	if err != nil || out == "" {
		return ""
	}
	out = strings.TrimSpace(out)
	return fmt.Sprintf("Recent commits:\n%s\n", out)
}

func (p *Prompt) Name() string {
	return p.name
}
