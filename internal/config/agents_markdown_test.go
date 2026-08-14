package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// writeAgent drops a markdown agent into <root>/<dir>/<file>.
func writeAgent(t *testing.T, root, dir, file, content string) {
	t.Helper()
	full := filepath.Join(root, dir)
	require.NoError(t, os.MkdirAll(full, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(full, file), []byte(content), 0o644))
}

// writeConfigFile drops a raw JSON config file into <root>/<name>.
func writeConfigFile(t *testing.T, root, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o644))
}

func TestDiscoverBraidAgent(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "reviewer.md", `---
name: reviewer
description: Reviews Go code
reasoning_effort: low
tools: [read, grep]
---
You review Go code.`)

	got := discoverMarkdownAgents(root, nil)

	agent, ok := got["reviewer"]
	require.True(t, ok)
	require.Equal(t, "Reviews Go code", agent.Description)
	require.Empty(t, agent.Model)
	require.Equal(t, "low", agent.ReasoningEffort)
	require.Equal(t, []string{"read", "grep"}, agent.AllowedTools)
	require.Equal(t, "You review Go code.", agent.Prompt)
}

// "large"/"small" carry no special meaning for an agent's model anymore, not
// even in Braid's own agent files: with no provider named "small", this
// resolves like any other unrecognised model and falls back to empty (the
// app's main model), with a debug log fired.
func TestDiscoverBraidAgentSlotWordsAreNotSpecial(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "reviewer.md", `---
name: reviewer
description: Reviews Go code
model: small
---
You review Go code.`)

	got := discoverMarkdownAgents(root, nil)

	agent, ok := got["reviewer"]
	require.True(t, ok)
	require.Empty(t, agent.Model)
}

// A foreign provider/model reference that resolves against a configured
// provider is honoured, normalized to "provider/model-id", when it appears
// in Braid's own agent directory (e.g. a hand-authored file, or one dropped
// there by `braid import`).
func TestDiscoverAgentResolvesForeignProviderModel(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "reviewer-dba.md", `---
name: reviewer-dba
description: Reviews SQL
model: fakeprovider/FAKE-MODEL
---
You review databases.`)

	providers := map[string]ProviderConfig{
		"fakeprovider": {
			ID:     "fakeprovider",
			Models: []catwalk.Model{{ID: "fake-model"}},
		},
	}

	got := discoverMarkdownAgents(root, providers)

	agent, ok := got["reviewer-dba"]
	require.True(t, ok)
	require.Equal(t, "fakeprovider/fake-model", agent.Model)
}

// Only .braid/agents is auto-discovered. Agent files written for other
// tools are not picked up on their own — they need `braid import` to bring
// them in (see TECHDEBT.md and cmd/import.go).
func TestDiscoverIgnoresForeignDirs(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".opencode/agent", "reviewer.md", "---\ndescription: from opencode\n---\nopencode body")
	writeAgent(t, root, ".claude/agents", "reviewer.md", "---\nname: reviewer\ndescription: from claude\n---\nclaude body")

	got := discoverMarkdownAgents(root, nil)
	require.NotContains(t, got, "reviewer")
}

func TestDiscoverPrefersBraidAgentsOverForeignDirs(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".opencode/agent", "reviewer.md", "---\ndescription: from opencode\n---\nopencode body")
	writeAgent(t, root, ".claude/agents", "reviewer.md", "---\nname: reviewer\ndescription: from claude\n---\nclaude body")
	writeAgent(t, root, ".braid/agents", "reviewer.md", "---\nname: reviewer\ndescription: from braid\n---\nbraid body")

	got := discoverMarkdownAgents(root, nil)
	require.Equal(t, "from braid", got["reviewer"].Description)
	require.Equal(t, "braid body", got["reviewer"].Prompt)
}

// mode: primary is still respected for files that live in .braid/agents
// itself (e.g. an imported opencode file that kept the field).
func TestDiscoverSkipsPrimaryMode(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "build.md", "---\nname: build\nmode: primary\ndescription: x\n---\nbody")

	require.NotContains(t, discoverMarkdownAgents(root, nil), "build")
}

// Tool names are no longer translated during regular discovery — that only
// happens in `braid import` now. A .braid/agents file naming Claude Code's
// tools verbatim keeps those names as-is, which grants nothing useful, but
// that's the user's file to fix (or re-run the importer).
func TestDiscoverBraidAgentDoesNotTranslateToolNames(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
tools: Read, Grep, Glob, Bash
---
You review.`)

	got := discoverMarkdownAgents(root, nil)
	require.Equal(t, []string{"Read", "Grep", "Glob", "Bash"}, got["reviewer"].AllowedTools)
}

func TestDiscoverSkipsDisabledAndBrokenFiles(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "off.md", "---\nname: off\ndisabled: true\n---\nbody")
	writeAgent(t, root, ".braid/agents", "nofm.md", "no frontmatter here")
	writeAgent(t, root, ".braid/agents", "unclosed.md", "---\nname: unclosed\ndescription: x\nbody")
	writeAgent(t, root, ".braid/agents", "empty.md", "---\nname: empty\n---\n   \n")
	writeAgent(t, root, ".braid/agents", "notes.txt", "---\nname: notes\n---\nbody")

	// Asserting on specific ids rather than an empty map: discovery also
	// scans the user's global agents directory, which the test cannot control.
	got := discoverMarkdownAgents(root, nil)
	for _, id := range []string{"off", "nofm", "unclosed", "empty", "notes"} {
		require.NotContains(t, got, id)
	}
}

func TestDiscoverIgnoresMissingDirs(t *testing.T) {
	require.NotPanics(t, func() { discoverMarkdownAgents(t.TempDir(), nil) })
	require.Empty(t, discoverMarkdownAgents("", nil), "an unknown working dir must not scan anything")
}

// Markdown files under .braid/agents are the only source of user-defined
// agents: a JSON "agents" entry of the same id must not win, or even be
// read at all. This is the direct regression test for the incident that
// prompted the JSON surface's removal — a braid agent once wrote a junk
// model into a *global* JSON agents block that silently shadowed a clean
// project markdown agent of the same name.
func TestSetupAgentsIgnoresJSONAgentsBlock(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "reviewer.md", "---\nname: reviewer\ndescription: from file\n---\nfile body")
	writeAgent(t, root, ".braid/agents", "dba.md", "---\nname: dba\ndescription: dba file\n---\ndba body")
	writeConfigFile(t, root, "braid.json", `{"agents":{"reviewer":{"description":"from json","prompt":"json body"}}}`)

	cfg, _, err := loadFromConfigPaths(context.Background(), []string{filepath.Join(root, "braid.json")})
	require.NoError(t, err)
	require.True(t, cfg.jsonAgentsBlockDetected, "loadFromBytes must record a top-level agents key")

	cfg.workingDir = root
	cfg.Options = &Options{}
	cfg.SetupAgents()

	require.Equal(t, "from file", cfg.Agents["reviewer"].Description, "the JSON entry must not override the markdown file")
	require.Equal(t, "file body", cfg.Agents["reviewer"].Prompt)
	require.Equal(t, "dba file", cfg.Agents["dba"].Description)
	require.Contains(t, cfg.Agents[AgentCoder].AllowedTools, "reviewer")
	require.Contains(t, cfg.Agents[AgentCoder].AllowedTools, "dba")

	var warned bool
	for _, p := range cfg.Problems {
		if p.Area == AreaAgent && p.Severity == SeverityWarn &&
			strings.Contains(p.Message, "agents in braid.json are ignored — define agents as .braid/agents/*.md files") {
			warned = true
		}
	}
	require.True(t, warned, "Doctor should surface a Problem for the ignored JSON agents block")
}

// Directly seeding cfg.Agents (bypassing loadFromBytes, the way a stale live
// Config would if SetupAgents still trusted its own previous output) must
// not survive a SetupAgents call either: only markdown discovery may
// populate c.Agents.
func TestSetupAgentsDoesNotTrustPreexistingAgentsField(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "reviewer.md", "---\nname: reviewer\ndescription: from file\n---\nfile body")

	cfg := newAgentConfig(t, `{"agents":{"reviewer":{"description":"stale","prompt":"stale body"}}}`)
	cfg.workingDir = root
	cfg.SetupAgents()

	require.Equal(t, "from file", cfg.Agents["reviewer"].Description)
}

func TestStringListAcceptsBothForms(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, ".braid/agents", "a.md", "---\nname: a\ntools: read, grep\n---\nbody")
	writeAgent(t, root, ".braid/agents", "b.md", "---\nname: b\ntools: [read, grep]\n---\nbody")

	got := discoverMarkdownAgents(root, nil)
	require.Equal(t, []string{"read", "grep"}, got["a"].AllowedTools)
	require.Equal(t, []string{"read", "grep"}, got["b"].AllowedTools)
}
