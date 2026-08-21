package prompt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/stretchr/testify/require"
)

// requireGit skips the test if git is not on PATH, matching the pattern used
// in internal/git and internal/config.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

// newStore builds a minimal ConfigStore rooted at dir, with a non-nil
// Options block so loadContextFiles/promptData don't have to nil-check it.
func newStore(t *testing.T, dir string) *config.ConfigStore {
	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{},
	}
	return config.NewTestStore(t, cfg, config.WithWorkingDir(dir))
}

func TestNewPrompt(t *testing.T) {
	t.Parallel()

	t.Run("sets name and template", func(t *testing.T) {
		t.Parallel()
		p, err := NewPrompt("test", "hello {{.Provider}}")
		require.NoError(t, err)
		require.Equal(t, "test", p.Name())
	})

	// NewPrompt only stores the template string; it does not parse it, so a
	// malformed template is accepted here and only fails later, in Build.
	t.Run("malformed template is accepted, not rejected", func(t *testing.T) {
		t.Parallel()
		p, err := NewPrompt("bad", "{{.Provider")
		require.NoError(t, err)

		store := newStore(t, t.TempDir())
		_, err = p.Build(context.Background(), "anthropic", "claude", store)
		require.Error(t, err)
	})
}

func TestPrompt_Options(t *testing.T) {
	t.Parallel()

	t.Run("WithTimeFunc controls Date deterministically", func(t *testing.T) {
		t.Parallel()
		fixed := time.Date(2020, 3, 4, 0, 0, 0, 0, time.UTC)
		p, err := NewPrompt("t", "{{.Date}}", WithTimeFunc(func() time.Time { return fixed }))
		require.NoError(t, err)

		store := newStore(t, t.TempDir())
		out, err := p.Build(context.Background(), "p", "m", store)
		require.NoError(t, err)
		require.Equal(t, "3/4/2020", out)
	})

	t.Run("WithPlatform overrides runtime.GOOS", func(t *testing.T) {
		t.Parallel()
		p, err := NewPrompt("t", "{{.Platform}}", WithPlatform("plan9"))
		require.NoError(t, err)

		store := newStore(t, t.TempDir())
		out, err := p.Build(context.Background(), "p", "m", store)
		require.NoError(t, err)
		require.Equal(t, "plan9", out)
	})

	t.Run("WithWorkingDir overrides the data working dir but not the git-repo check", func(t *testing.T) {
		t.Parallel()
		nonGitDir := t.TempDir()
		override := filepath.ToSlash(filepath.Join(t.TempDir(), "override"))
		p, err := NewPrompt("t", "{{.WorkingDir}}|{{.IsGitRepo}}", WithWorkingDir(override))
		require.NoError(t, err)

		store := newStore(t, nonGitDir)
		out, err := p.Build(context.Background(), "p", "m", store)
		require.NoError(t, err)
		// WorkingDir in the rendered data reflects the override, but the
		// git-repo check is always against the store's own working dir.
		require.Equal(t, override+"|false", out)
	})

	t.Run("no WithWorkingDir falls back to store working dir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p, err := NewPrompt("t", "{{.WorkingDir}}")
		require.NoError(t, err)

		store := newStore(t, dir)
		out, err := p.Build(context.Background(), "p", "m", store)
		require.NoError(t, err)
		require.Equal(t, filepath.ToSlash(dir), out)
	})
}

func TestBuild(t *testing.T) {
	t.Parallel()

	t.Run("renders provider and model", func(t *testing.T) {
		t.Parallel()
		p, err := NewPrompt("t", "{{.Provider}}/{{.Model}}")
		require.NoError(t, err)

		store := newStore(t, t.TempDir())
		out, err := p.Build(context.Background(), "anthropic", "claude-x", store)
		require.NoError(t, err)
		require.Equal(t, "anthropic/claude-x", out)
	})

	t.Run("parse error is returned, not panicked", func(t *testing.T) {
		t.Parallel()
		p, err := NewPrompt("t", "{{.Provider")
		require.NoError(t, err)

		store := newStore(t, t.TempDir())
		_, err = p.Build(context.Background(), "p", "m", store)
		require.Error(t, err)
		require.Contains(t, err.Error(), "parsing template")
	})

	t.Run("execute error on unknown field is returned", func(t *testing.T) {
		t.Parallel()
		p, err := NewPrompt("t", "{{.NoSuchField}}")
		require.NoError(t, err)

		store := newStore(t, t.TempDir())
		_, err = p.Build(context.Background(), "p", "m", store)
		require.Error(t, err)
		require.Contains(t, err.Error(), "executing template")
	})

	t.Run("renders context files", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "NOTES.md"), []byte("hello notes"), 0o644))

		cfg := &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Options:   &config.Options{ContextPaths: []string{"NOTES.md"}},
		}
		store := config.NewTestStore(t, cfg, config.WithWorkingDir(dir))

		p, err := NewPrompt("t", "{{range .ContextFiles}}{{.Path}}={{.Content}}{{end}}")
		require.NoError(t, err)
		out, err := p.Build(context.Background(), "p", "m", store)
		require.NoError(t, err)
		require.Contains(t, out, "hello notes")
	})
}

func TestExpandPath(t *testing.T) {
	t.Parallel()

	store := newStore(t, t.TempDir())

	t.Run("empty path stays empty", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "", expandPath("", store))
	})

	t.Run("absolute path is unchanged", func(t *testing.T) {
		t.Parallel()
		abs := filepath.Join(t.TempDir(), "foo")
		require.Equal(t, abs, expandPath(abs, store))
	})

	t.Run("relative path is unchanged", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "relative/path.md", expandPath("relative/path.md", store))
	})

	t.Run("tilde is expanded against the real home dir", func(t *testing.T) {
		t.Parallel()
		got := expandPath("~/notes.md", store)
		require.Equal(t, filepath.Join(home.Dir(), "notes.md"), got)
	})

	t.Run("dollar var embedded mid-path is left alone", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "foo/$BAR", expandPath("foo/$BAR", store))
	})
}

// TestExpandPath_DollarVar is split out from TestExpandPath because
// t.Setenv cannot be used once any ancestor test has called t.Parallel.
func TestExpandPath_DollarVar(t *testing.T) {
	store := newStore(t, t.TempDir())
	t.Setenv("SENNIT_PROMPT_TEST_VAR", "/resolved/value")
	got := expandPath("$SENNIT_PROMPT_TEST_VAR", store)
	require.Equal(t, "/resolved/value", got)
}

func TestProcessFile(t *testing.T) {
	t.Parallel()

	t.Run("existing file returns its content", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, "a.txt")
		require.NoError(t, os.WriteFile(p, []byte("content-a"), 0o644))

		got := processFile(p)
		require.NotNil(t, got)
		require.Equal(t, p, got.Path)
		require.Equal(t, "content-a", got.Content)
	})

	t.Run("missing file returns nil", func(t *testing.T) {
		t.Parallel()
		got := processFile(filepath.Join(t.TempDir(), "missing.txt"))
		require.Nil(t, got)
	})

	t.Run("directory returns nil (ReadFile fails on a dir)", func(t *testing.T) {
		t.Parallel()
		got := processFile(t.TempDir())
		require.Nil(t, got)
	})
}

func TestProcessContextPath(t *testing.T) {
	t.Parallel()

	t.Run("single existing file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "f.md"), []byte("f-content"), 0o644))
		store := newStore(t, dir)

		got := processContextPath("f.md", store)
		require.Len(t, got, 1)
		require.Equal(t, "f-content", got[0].Content)
	})

	t.Run("directory walks and collects nested files", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "docs"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "one.md"), []byte("one"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "two.md"), []byte("two"), 0o644))
		store := newStore(t, dir)

		got := processContextPath("docs", store)
		require.Len(t, got, 2)
		contents := []string{got[0].Content, got[1].Content}
		require.ElementsMatch(t, []string{"one", "two"}, contents)
	})

	t.Run("missing path returns no entries", func(t *testing.T) {
		t.Parallel()
		store := newStore(t, t.TempDir())
		got := processContextPath("does-not-exist.md", store)
		require.Empty(t, got)
	})

	t.Run("unreadable file is skipped, not fatal", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("chmod-based unreadable file simulation is unix-only")
		}
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, "secret.md")
		require.NoError(t, os.WriteFile(p, []byte("nope"), 0o644))
		require.NoError(t, os.Chmod(p, 0o000))
		t.Cleanup(func() { _ = os.Chmod(p, 0o644) })

		// Root (and some CI containers) can read files regardless of mode;
		// only assert the no-permission behavior when it actually bites.
		if os.Geteuid() == 0 {
			t.Skip("running as root: chmod 0000 does not block reads")
		}

		store := newStore(t, dir)
		got := processContextPath("secret.md", store)
		require.Empty(t, got)
	})
}

func TestLoadContextFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("A"), 0o644))
	store := newStore(t, dir)

	t.Run("loads each configured path", func(t *testing.T) {
		t.Parallel()
		files := loadContextFiles([]string{"a.md"}, store)
		require.Len(t, files, 1)
		require.Len(t, files["a.md"], 1)
		require.Equal(t, "A", files["a.md"][0].Content)
	})

	t.Run("deduplicates case-insensitively", func(t *testing.T) {
		t.Parallel()
		files := loadContextFiles([]string{"a.md", "A.MD"}, store)
		// Both paths normalize to the same map key, so only one entry.
		require.Len(t, files, 1)
	})

	t.Run("a path that expands to nothing yields an empty slice, not an error", func(t *testing.T) {
		t.Parallel()
		files := loadContextFiles([]string{"missing.md"}, store)
		require.Contains(t, files, "missing.md")
		require.Empty(t, files["missing.md"])
	})
}

func TestIsGitRepo(t *testing.T) {
	t.Parallel()

	t.Run("directory with a .git entry is a repo", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
		require.True(t, isGitRepo(dir))
	})

	t.Run("plain directory is not a repo", func(t *testing.T) {
		t.Parallel()
		require.False(t, isGitRepo(t.TempDir()))
	})
}

// initGitRepo creates a scratch git repo with one commit, so the branch and
// log helpers have something real to report.
func initGitRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)

	dir := t.TempDir()
	ctx := t.Context()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644))
	run("add", "-A")
	run("commit", "-m", "initial commit")
	return dir
}

func TestGetGitStatus(t *testing.T) {
	t.Parallel()

	t.Run("in a git repo, branch and clean status are reported", func(t *testing.T) {
		t.Parallel()
		dir := initGitRepo(t)
		got := getGitStatus(t.Context(), dir)
		require.Contains(t, got, "Current branch: main")
		require.Contains(t, got, "Status: clean")
		require.Contains(t, got, "Recent commits:")
		require.Contains(t, got, "initial commit")
	})

	t.Run("in a git repo with uncommitted changes, status lists them", func(t *testing.T) {
		t.Parallel()
		dir := initGitRepo(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644))
		got := getGitStatus(t.Context(), dir)
		require.Contains(t, got, "Status:")
		require.Contains(t, got, "dirty.txt")
	})

	t.Run("outside a git repo, branch and commits are silent, and status says so rather than asserting clean", func(t *testing.T) {
		requireGit(t)
		t.Parallel()
		dir := t.TempDir()
		got := getGitStatus(t.Context(), dir)
		// getGitBranch and getGitRecentCommits run "git ... 2>/dev/null"
		// directly, so a non-zero git exit (not a repo) is observed and
		// they contribute nothing. getGitStatusSummary now runs "git
		// status" the same way — no more "| head -20" masking git's own
		// exit status behind head's — so a failing "git status" here (no
		// repo at all) is observed too, and reported as "could not be
		// determined" rather than misread as an empty, clean status.
		require.Equal(t, "Status: could not be determined (git failed)\n", got)
	})

	t.Run("status is truncated to 20 lines without losing git's own exit status", func(t *testing.T) {
		t.Parallel()
		dir := initGitRepo(t)
		for i := range 25 {
			require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("dirty-%02d.txt", i)), []byte("x"), 0o644))
		}
		got := getGitStatus(t.Context(), dir)
		require.Equal(t, 20, strings.Count(got, "dirty-"), "status must be capped at 20 files, truncated in Go now that git's own exit status is what err reflects")
	})
}

func TestPromptData(t *testing.T) {
	t.Parallel()

	t.Run("non-git working dir degrades gracefully", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		store := newStore(t, dir)
		p, err := NewPrompt("t", "")
		require.NoError(t, err)

		data := p.promptData(context.Background(), "anthropic", "claude", store)
		require.False(t, data.IsGitRepo)
		require.Empty(t, data.GitStatus)
		require.Equal(t, "anthropic", data.Provider)
		require.Equal(t, "claude", data.Model)
		require.Equal(t, filepath.ToSlash(dir), data.WorkingDir)
	})

	t.Run("git working dir populates GitStatus", func(t *testing.T) {
		dir := initGitRepo(t)
		store := newStore(t, dir)
		p, err := NewPrompt("t", "")
		require.NoError(t, err)

		data := p.promptData(context.Background(), "anthropic", "claude", store)
		require.True(t, data.IsGitRepo)
		require.Contains(t, data.GitStatus, "Current branch: main")
	})

	t.Run("a configured user skill overriding a builtin name is discovered and wins", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		skillsDir := filepath.Join(dir, "myskills", "jq")
		require.NoError(t, os.MkdirAll(skillsDir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(skillsDir, "SKILL.md"),
			[]byte("---\nname: jq\ndescription: overridden jq skill for this test.\n---\n\nbody\n"),
			0o644,
		))

		// An absolute entry must survive untouched: SmartJoin returns an
		// absolute path unchanged. The relative case is covered by
		// TestPromptData_RelativeSkillsPathResolvesAgainstWorkspace below.
		cfg := &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Options:   &config.Options{SkillsPaths: []string{filepath.Join(dir, "myskills")}},
		}
		store := config.NewTestStore(t, cfg, config.WithWorkingDir(dir))
		p, err := NewPrompt("t", "")
		require.NoError(t, err)

		data := p.promptData(context.Background(), "anthropic", "claude", store)
		require.Contains(t, data.AvailSkillXML, "overridden jq skill for this test.")
	})
}

// TestPromptData_RelativeSkillsPathResolvesAgainstWorkspace pins the fix for
// a relative options.skills_paths entry. It used to go through expandPath
// alone, which handles ~ and $VAR but joins nothing, so the entry resolved
// against whatever directory the process was started in -- a different answer
// depending on how sennit was launched. Context paths never had this problem
// because processContextPath SmartJoins them against the working dir; skills
// paths now do the same.
func TestPromptData_RelativeSkillsPathResolvesAgainstWorkspace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "team-skills", "deploy")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: deploy\ndescription: a workspace-relative skill.\n---\n\nbody\n"),
		0o644,
	))

	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		// Relative on purpose: it only resolves if it is anchored to the
		// store's working dir, which is dir and is never the test binary's
		// cwd.
		Options: &config.Options{SkillsPaths: []string{"team-skills"}},
	}
	store := config.NewTestStore(t, cfg, config.WithWorkingDir(dir))

	p, err := NewPrompt("t", "")
	require.NoError(t, err)

	data := p.promptData(context.Background(), "anthropic", "claude", store)
	require.Contains(t, data.AvailSkillXML, "a workspace-relative skill.",
		"a relative skills_paths entry must resolve against the workspace, "+
			"not the process's cwd")
}
