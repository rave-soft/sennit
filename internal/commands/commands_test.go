package commands

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestLoadFromSource_NonExistentDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Empty(t, cmds)

	// directory must NOT have been created
	_, statErr := os.Stat(dir)
	require.True(t, os.IsNotExist(statErr))
}

func TestLoadFromSource_ExistingDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.md"), []byte("say hello"), 0o644))

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, "user:hello", cmds[0].ID)
	require.Equal(t, "say hello", cmds[0].Content)
}

func TestLoadAll_MixedSources(t *testing.T) {
	t.Parallel()

	existing := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(existing, "cmd.md"), []byte("content"), 0o644))

	missing := filepath.Join(t.TempDir(), "nope")

	cmds, err := loadAll([]commandSource{
		{path: existing, prefix: userCommandPrefix},
		{path: missing, prefix: projectCommandPrefix},
	})
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, "user:cmd", cmds[0].ID)
}

func TestFromSkillCatalog_UserInvocableOnly(t *testing.T) {
	t.Parallel()

	cmds := FromSkillCatalog([]skills.CatalogEntry{
		{
			ID:            "/skills/on/SKILL.md",
			Name:          "on",
			Description:   "Enabled.",
			Label:         "user:on",
			UserInvocable: true,
		},
		{
			ID:            "/skills/off/SKILL.md",
			Name:          "off",
			Description:   "Not invocable.",
			Label:         "user:off",
			UserInvocable: false,
		},
	})

	require.Len(t, cmds, 1)
	require.Equal(t, "user:on", cmds[0].ID)
	require.Equal(t, "user:on", cmds[0].Name)
	require.Equal(t, "on", cmds[0].Skill.Name)
	require.Equal(t, "Enabled.", cmds[0].Skill.Description)
	require.Equal(t, "/skills/on/SKILL.md", cmds[0].Skill.SkillFilePath)
}

func TestFromSkillCatalog_UsesDiscoveredSymlinkedSkills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires special privileges on Windows")
	}
	t.Parallel()

	root := t.TempDir()
	targetParent := t.TempDir()
	targetSkillDir := filepath.Join(targetParent, "linked-skill")
	require.NoError(t, os.MkdirAll(targetSkillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(targetSkillDir, skills.SkillFileName),
		[]byte("---\nname: linked-skill\ndescription: Symlinked.\nuser-invocable: true\n---\nUse me.\n"),
		0o644,
	))

	link := filepath.Join(root, "linked-skill")
	require.NoError(t, os.Symlink(targetSkillDir, link))

	_, activeSkills, _ := skills.DiscoverFromConfig(skills.DiscoveryConfig{
		SkillsPaths: []string{root},
	})
	entries := skills.Catalog(activeSkills, []string{root}, "")
	cmds := FromSkillCatalog(entries)

	require.Len(t, cmds, 1)
	require.Equal(t, "user:linked-skill", cmds[0].ID)
	require.Equal(t, "linked-skill", cmds[0].Skill.Name)
	require.Equal(t, filepath.Join(link, skills.SkillFileName), cmds[0].Skill.SkillFilePath)
}

// seqOf builds an iter.Seq2 from a plain map, the shape LoadMCPPrompts now
// takes in place of *mcp.Registry (B1: this package must not depend on
// internal/agent/tools/mcp just to read two methods off it).
func seqOf(catalog map[string][]*sdkmcp.Prompt) iter.Seq2[string, []*sdkmcp.Prompt] {
	return func(yield func(string, []*sdkmcp.Prompt) bool) {
		for name, prompts := range catalog {
			if !yield(name, prompts) {
				return
			}
		}
	}
}

// TestLoadMCPPrompts_ConvertsCatalog pins the shape LoadMCPPrompts builds
// from a prompt catalog, including argument title fallback to name.
func TestLoadMCPPrompts_ConvertsCatalog(t *testing.T) {
	t.Parallel()

	catalog := seqOf(map[string][]*sdkmcp.Prompt{
		"github": {
			{
				Name:        "review",
				Title:       "Review PR",
				Description: "Reviews a pull request",
				Arguments: []*sdkmcp.PromptArgument{
					{Name: "pr", Title: "Pull request", Required: true},
					{Name: "verbose", Description: "Verbose output"},
				},
			},
		},
	})

	got, err := LoadMCPPrompts(catalog)
	require.NoError(t, err)
	require.Len(t, got, 1)

	prompt := got[0]
	require.Equal(t, "github:review", prompt.ID)
	require.Equal(t, "review", prompt.PromptID)
	require.Equal(t, "github", prompt.ClientID)
	require.Equal(t, "Review PR", prompt.Title)
	require.Equal(t, []Argument{
		{ID: "pr", Title: "Pull request", Required: true},
		{ID: "verbose", Title: "verbose", Description: "Verbose output"},
	}, prompt.Arguments)
}

// TestLoadMCPPrompts_NilSequence covers the caller with no MCP prompts to
// offer, mirroring the old *mcp.Registry nil-registry guard.
func TestLoadMCPPrompts_NilSequence(t *testing.T) {
	t.Parallel()

	got, err := LoadMCPPrompts(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestGetMCPPrompt_DelegatesAndJoins pins GetMCPPrompt's new shape: a
// closure over the registry call (B1) rather than *mcp.Registry and
// mcp.ConfigProvider directly, invoked with the same clientID/promptID/
// args and joining the returned message parts with a space.
func TestGetMCPPrompt_DelegatesAndJoins(t *testing.T) {
	t.Parallel()

	var gotClientID, gotPromptID string
	var gotArgs map[string]string
	fetch := func(ctx context.Context, clientID, promptID string, args map[string]string) ([]string, error) {
		gotClientID, gotPromptID, gotArgs = clientID, promptID, args
		return []string{"hello", "world"}, nil
	}

	got, err := GetMCPPrompt(fetch, "github", "review", map[string]string{"pr": "42"})
	require.NoError(t, err)
	require.Equal(t, "hello world", got)
	require.Equal(t, "github", gotClientID)
	require.Equal(t, "review", gotPromptID)
	require.Equal(t, map[string]string{"pr": "42"}, gotArgs)
}

// TestGetMCPPrompt_PropagatesError covers the closure's failure path.
func TestGetMCPPrompt_PropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	fetch := func(ctx context.Context, clientID, promptID string, args map[string]string) ([]string, error) {
		return nil, wantErr
	}

	_, err := GetMCPPrompt(fetch, "github", "review", nil)
	require.ErrorIs(t, err, wantErr)
}
