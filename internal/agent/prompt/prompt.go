package prompt

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
)

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

func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore) (string, error) {
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
		return nil
	}
	return &ContextFile{
		Path:    filePath,
		Content: string(content),
	}
}

func processContextPath(p string, store *config.ConfigStore) []ContextFile {
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
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of paths.
func loadContextFiles(paths []string, store *config.ConfigStore) map[string][]ContextFile {
	files := map[string][]ContextFile{}
	for _, pth := range paths {
		expanded := expandPath(pth, store)
		pathKey := strings.ToLower(expanded)
		if _, ok := files[pathKey]; ok {
			continue
		}
		files[pathKey] = processContextPath(expanded, store)
	}
	return files
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore) PromptDat {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	cfg := store.Config()
	contextFiles := loadContextFiles(cfg.Options.ContextPaths, store)
	globalContextFiles := loadContextFiles(cfg.Options.GlobalContextPaths, store)

	// Discover and load skills metadata.
	var availSkillXML string

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

	isGit := isGitRepo(store.WorkingDir())
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
	}
	if isGit {
		data.GitStatus = getGitStatus(ctx, store.WorkingDir())
	}

	for _, files := range contextFiles {
		data.ContextFiles = append(data.ContextFiles, files...)
	}
	for _, files := range globalContextFiles {
		data.GlobalContextFiles = append(data.GlobalContextFiles, files...)
	}
	return data
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
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
