package importer

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// writeForeignAgent drops a foreign agent markdown file at
// <root>/<sourceDir>/<file>, mirroring the shape sennit import reads.
func writeForeignAgent(t *testing.T, root, sourceDir, file, content string) {
	t.Helper()
	dir := filepath.Join(root, sourceDir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644))
}

// writeForeignSkill drops a SKILL.md at
// <root>/<sourceDir>/<name>/SKILL.md.
func writeForeignSkill(t *testing.T, root, sourceDir, name, content string) {
	t.Helper()
	dir := filepath.Join(root, sourceDir, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}

func findEntry(t *testing.T, report Report, kind, name string) Entry {
	t.Helper()
	for _, e := range report.Entries {
		if e.Kind == kind && e.Name == name {
			return e
		}
	}
	t.Fatalf("no %s entry named %q in report: %+v", kind, name, report.Entries)
	return Entry{}
}

// An unresolvable model (a Claude Code model name, not a configured
// provider/model) is reported as a warning, dropped from the written
// frontmatter, and left as a comment rather than silently vanishing.
func TestRunImport_ClaudeAgent_ModelNotAvailable(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
model: claude-opus-4
---
You review code.`)

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusAdjusted, entry.Status)
	require.NotEmpty(t, entry.Warnings)
	require.Contains(t, entry.Warnings[0], "claude-opus-4")

	written, err := os.ReadFile(filepath.Join(root, ".sennit", "agents", "reviewer.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "# original model: claude-opus-4 — not available")
	require.NotContains(t, string(written), "\nmodel: ", "the dropped model must not also appear as a real frontmatter field")
}

// A model that does resolve is written normalized to "provider/model-id",
// with no warning.
func TestRunImport_ClaudeAgent_ModelResolves(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
model: fakeprovider/FAKE-MODEL
---
You review code.`)

	providers := map[string]config.ProviderConfig{
		"fakeprovider": {ID: "fakeprovider", Models: []catwalk.Model{{ID: "fake-model"}}},
	}

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Agents: true, Providers: providers,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusImported, entry.Status)
	require.Empty(t, entry.Warnings)

	written, err := os.ReadFile(filepath.Join(root, ".sennit", "agents", "reviewer.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "model: fakeprovider/fake-model")
}

// Claude Code's tool names are translated to Sennit's own.
func TestRunImport_ClaudeAgent_ToolTranslation(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
tools: Read, Grep, Bash
---
You review code.`)

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusImported, entry.Status)

	written, err := os.ReadFile(filepath.Join(root, ".sennit", "agents", "reviewer.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "tools:")
	require.Contains(t, string(written), "view")
	require.Contains(t, string(written), "grep")
	require.Contains(t, string(written), "bash")
}

// A tool name with no Sennit equivalent is dropped and reported, not kept
// verbatim.
func TestRunImport_Agent_UnknownToolDropped(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
tools: Read, WebSearch
---
You review code.`)

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusAdjusted, entry.Status)
	require.Contains(t, entry.Warnings[0], "WebSearch")

	written, err := os.ReadFile(filepath.Join(root, ".sennit", "agents", "reviewer.md"))
	require.NoError(t, err)
	require.NotContains(t, string(written), "WebSearch")
}

// opencode's permission block is not enforced — it must be reported and
// dropped, never silently written into a Sennit agent file (there's nowhere
// for it to go).
func TestRunImport_OpenCodeAgent_PermissionWarning(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".opencode/agent", "dba.md", `---
description: Reviews SQL
permission:
  edit: deny
  bash: ask
---
You review databases.`)

	report, err := Run(Options{
		Source: SourceOpenCode, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "dba")
	require.Equal(t, StatusAdjusted, entry.Status)
	found := false
	for _, w := range entry.Warnings {
		if w == "permission block is not supported; restrict tools via the tools list instead" {
			found = true
		}
	}
	require.True(t, found, "expected a permission warning, got %v", entry.Warnings)

	written, err := os.ReadFile(filepath.Join(root, ".sennit", "agents", "dba.md"))
	require.NoError(t, err)
	require.NotContains(t, string(written), "permission:")
	require.Contains(t, string(written), "# original permission block dropped")
}

// An out-of-range reasoning effort maps to the nearest valid level with a
// warning, rather than being kept verbatim or silently dropped.
func TestRunImport_Agent_ReasoningEffortMapped(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".opencode/agent", "dba.md", `---
description: Reviews SQL
effort: max
---
You review databases.`)

	report, err := Run(Options{
		Source: SourceOpenCode, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "dba")
	require.Equal(t, StatusAdjusted, entry.Status)

	written, err := os.ReadFile(filepath.Join(root, ".sennit", "agents", "dba.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "reasoning_effort: high")
}

// A SKILL.md that already meets the Agent Skills spec is copied verbatim,
// directory and all.
func TestRunImport_Skill_Copied(t *testing.T) {
	root := t.TempDir()
	writeForeignSkill(t, root, ".claude/skills", "pdf-fill", `---
name: pdf-fill
description: Fill PDF forms.
---
Fill the form.`)

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Skills: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "skill", "pdf-fill")
	require.Equal(t, StatusImported, entry.Status)

	written, err := os.ReadFile(filepath.Join(root, ".sennit", "skills", "pdf-fill", "SKILL.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "Fill the form.")
}

// A symlink inside a foreign skill directory must not be followed: os.ReadFile
// (unlike fs.WalkDir) follows symlinks at the OS level, so copyDir has to
// filter them out explicitly or a symlink pointing outside the skill
// directory would leak its target's bytes into the destination.
func TestRunImport_Skill_SymlinkNotFollowed(t *testing.T) {
	root := t.TempDir()
	writeForeignSkill(t, root, ".claude/skills", "pdf-fill", `---
name: pdf-fill
description: Fill PDF forms.
---
Fill the form.`)

	outside := filepath.Join(root, "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("super secret contents"), 0o644))

	link := filepath.Join(root, ".claude", "skills", "pdf-fill", "notes.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Skills: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "skill", "pdf-fill")
	require.Equal(t, StatusAdjusted, entry.Status)
	require.NotEmpty(t, entry.Warnings)
	require.Contains(t, entry.Warnings[0], "notes.txt")

	dest := filepath.Join(root, ".sennit", "skills", "pdf-fill", "notes.txt")
	_, err = os.Stat(dest)
	require.True(t, os.IsNotExist(err), "symlink target must not be copied into the destination")

	entries, err := os.ReadDir(filepath.Join(root, ".sennit", "skills", "pdf-fill"))
	require.NoError(t, err)
	for _, e := range entries {
		content, err := os.ReadFile(filepath.Join(root, ".sennit", "skills", "pdf-fill", e.Name()))
		require.NoError(t, err)
		require.NotContains(t, string(content), "super secret contents")
	}
}

// A skill whose name doesn't match its directory fails Sennit's own
// validation and is skipped with a reason, not partially written.
func TestRunImport_Skill_InvalidIsSkipped(t *testing.T) {
	root := t.TempDir()
	writeForeignSkill(t, root, ".claude/skills", "pdf-fill", `---
name: totally-different-name
description: Fill PDF forms.
---
Fill the form.`)

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Skills: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "skill", "pdf-fill")
	require.Equal(t, StatusSkipped, entry.Status)
	require.NotEmpty(t, entry.Reason)

	_, err = os.Stat(filepath.Join(root, ".sennit", "skills", "totally-different-name"))
	require.True(t, os.IsNotExist(err))
}

// --dry-run reports the same outcome but writes nothing to disk.
func TestRunImport_DryRun_DoesNotWrite(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
---
You review code.`)
	writeForeignSkill(t, root, ".claude/skills", "pdf-fill", `---
name: pdf-fill
description: Fill PDF forms.
---
Fill the form.`)

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Agents: true, Skills: true, DryRun: true,
	})
	require.NoError(t, err)

	require.Equal(t, StatusImported, findEntry(t, report, "agent", "reviewer").Status)
	require.Equal(t, StatusImported, findEntry(t, report, "skill", "pdf-fill").Status)

	_, err = os.Stat(filepath.Join(root, ".sennit", "agents", "reviewer.md"))
	require.True(t, os.IsNotExist(err), "dry-run must not write the agent file")
	_, err = os.Stat(filepath.Join(root, ".sennit", "skills", "pdf-fill"))
	require.True(t, os.IsNotExist(err), "dry-run must not write the skill directory")
}

// Re-running an import without --force is a no-op against files it already
// wrote: idempotent, and it never clobbers a hand-edit made after import.
func TestRunImport_Idempotent_NoForce_SkipsExisting(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
---
You review code.`)

	opts := Options{Source: SourceClaude, WorkingDir: root, Agents: true}

	_, err := Run(opts)
	require.NoError(t, err)

	dest := filepath.Join(root, ".sennit", "agents", "reviewer.md")
	require.NoError(t, os.WriteFile(dest, []byte("hand-edited"), 0o644))

	report, err := Run(opts)
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusSkipped, entry.Status)
	require.Contains(t, entry.Reason, "already exists")

	written, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "hand-edited", string(written), "must not overwrite without --force")
}

// --force overwrites an existing destination file.
func TestRunImport_Force_Overwrites(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
---
You review code.`)

	opts := Options{Source: SourceClaude, WorkingDir: root, Agents: true}
	_, err := Run(opts)
	require.NoError(t, err)

	dest := filepath.Join(root, ".sennit", "agents", "reviewer.md")
	require.NoError(t, os.WriteFile(dest, []byte("hand-edited"), 0o644))

	opts.Force = true
	report, err := Run(opts)
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusImported, entry.Status)

	written, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Contains(t, string(written), "You review code.")
}

// A missing source directory just means nothing of that kind to import, not
// an error.
func TestRunImport_MissingSourceDirIsNotAnError(t *testing.T) {
	root := t.TempDir()

	report, err := Run(Options{
		Source: SourceOpenCode, WorkingDir: root, Agents: true, Skills: true,
	})
	require.NoError(t, err)
	require.Empty(t, report.Entries)
}

// opencode's disabled/primary agents are skipped, same as regular
// discovery does for .sennit/agents.
func TestRunImport_OpenCodeAgent_SkipsPrimaryAndDisabled(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".opencode/agent", "build.md", "---\nmode: primary\ndescription: x\n---\nbody")
	writeForeignAgent(t, root, ".opencode/agent", "off.md", "---\nname: off\ndisabled: true\ndescription: x\n---\nbody")

	report, err := Run(Options{
		Source: SourceOpenCode, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	require.Equal(t, StatusSkipped, findEntry(t, report, "agent", "build").Status)
	require.Equal(t, StatusSkipped, findEntry(t, report, "agent", "off").Status)
}

func TestRunImport_RejectsUnknownSource(t *testing.T) {
	_, err := Run(Options{Source: "cursor", WorkingDir: t.TempDir(), Agents: true})
	require.Error(t, err)
}

func TestRunImport_RequiresSkillsOrAgents(t *testing.T) {
	_, err := Run(Options{Source: SourceClaude, WorkingDir: t.TempDir()})
	require.Error(t, err)
}

// opencode reads both spellings of each directory — <dir>/skill and
// <dir>/skills for skills, <dir>/agent and <dir>/agents for agents —
// verified against a real installation (v1.18.18) by dropping probes in
// each and asking `opencode agent list` what it found. Importing only
// one spelling would silently miss whichever half of its users picked
// the other.
func TestRunImport_OpenCode_ReadsBothDirectorySpellings(t *testing.T) {
	root := t.TempDir()
	writeForeignSkill(t, root, ".opencode/skills", "plural-skill", `---
name: plural-skill
description: Lives in the plural directory.
---
body`)
	writeForeignSkill(t, root, ".opencode/skill", "singular-skill", `---
name: singular-skill
description: Lives in the singular directory.
---
body`)
	writeForeignAgent(t, root, ".opencode/agent", "singular-agent.md", `---
name: singular-agent
description: Lives in the singular directory.
mode: subagent
---
body`)
	writeForeignAgent(t, root, ".opencode/agents", "plural-agent.md", `---
name: plural-agent
description: Lives in the plural directory.
mode: subagent
---
body`)

	report, err := Run(Options{
		Source: SourceOpenCode, WorkingDir: root, Skills: true, Agents: true,
	})
	require.NoError(t, err)

	for _, name := range []string{"plural-skill", "singular-skill"} {
		require.Equal(t, StatusImported, findEntry(t, report, "skill", name).Status)
		require.FileExists(t, filepath.Join(root, ".sennit", "skills", name, "SKILL.md"))
	}
	for _, name := range []string{"singular-agent", "plural-agent"} {
		require.Equal(t, StatusImported, findEntry(t, report, "agent", name).Status)
		require.FileExists(t, filepath.Join(root, ".sennit", "agents", name+".md"))
	}
}

// The same name in both spellings is one skill, not two. It is claimed
// by the first directory searched and reported as a skip against the
// other, so the user can see the duplicate rather than wonder which copy
// landed.
func TestRunImport_OpenCode_DuplicateAcrossSpellingsImportedOnce(t *testing.T) {
	root := t.TempDir()
	writeForeignSkill(t, root, ".opencode/skills", "shared", `---
name: shared
description: The copy in the plural directory.
---
plural body`)
	writeForeignSkill(t, root, ".opencode/skill", "shared", `---
name: shared
description: The copy in the singular directory.
---
singular body`)

	report, err := Run(Options{
		Source: SourceOpenCode, WorkingDir: root, Skills: true,
	})
	require.NoError(t, err)

	var imported, skipped int
	for _, e := range report.Entries {
		switch e.Status {
		case StatusImported:
			imported++
		case StatusSkipped:
			skipped++
			require.Contains(t, e.Reason, "already imported from")
		}
	}
	require.Equal(t, 1, imported, "one skill, however many directories hold it")
	require.Equal(t, 1, skipped, "the duplicate is reported, not silently dropped")

	written, err := os.ReadFile(filepath.Join(root, ".sennit", "skills", "shared", "SKILL.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "plural body", "the canonical directory wins")
}

// A "nothing to import" result has to name where it looked. The two
// causes — an empty setup and a wrong path — call for opposite next
// moves, and the report is the only place a user can tell them apart.
func TestRunImport_ReportsEveryDirectorySearched(t *testing.T) {
	root := t.TempDir()

	report, err := Run(Options{
		Source: SourceOpenCode, WorkingDir: root, Skills: true, Agents: true,
	})
	require.NoError(t, err)
	require.Empty(t, report.Entries)
	require.Equal(t, []string{
		filepath.Join(root, ".opencode", "skills"),
		filepath.Join(root, ".opencode", "skill"),
		filepath.Join(root, ".opencode", "agent"),
		filepath.Join(root, ".opencode", "agents"),
	}, report.Searched, "every directory looked in, existing or not")
}

// Only the kinds actually asked for are searched, so the list never
// implies a lookup that did not happen.
func TestRunImport_SearchedCoversOnlyRequestedKinds(t *testing.T) {
	root := t.TempDir()

	report, err := Run(Options{
		Source: SourceClaude, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(root, ".claude", "agents")}, report.Searched)
}

// opencode's global root is $XDG_CONFIG_HOME/opencode, falling back to
// ~/.config/opencode — the same resolution home.Config performs, checked
// against a real installation by pointing XDG_CONFIG_HOME at a fixture
// and confirming opencode loaded the agents and skills from it.
func TestImportSourceDirs_OpenCodeGlobalFollowsXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	skillsDirs, agentsDirs := importSourceDirs(SourceOpenCode, "/irrelevant", true)
	require.Equal(t, []string{
		filepath.Join(xdg, "opencode", "skills"),
		filepath.Join(xdg, "opencode", "skill"),
	}, skillsDirs)
	require.Equal(t, []string{
		filepath.Join(xdg, "opencode", "agent"),
		filepath.Join(xdg, "opencode", "agents"),
	}, agentsDirs)
}
