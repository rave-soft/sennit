package config

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// writeForeignAgent drops a foreign agent markdown file at
// <root>/<sourceDir>/<file>, mirroring the shape braid import reads.
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

func findEntry(t *testing.T, report ImportReport, kind, name string) ImportEntry {
	t.Helper()
	for _, e := range report.Entries {
		if e.Kind == kind && e.Name == name {
			return e
		}
	}
	t.Fatalf("no %s entry named %q in report: %+v", kind, name, report.Entries)
	return ImportEntry{}
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

	report, err := RunImport(ImportOptions{
		Source: ImportSourceClaude, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusAdjusted, entry.Status)
	require.NotEmpty(t, entry.Warnings)
	require.Contains(t, entry.Warnings[0], "claude-opus-4")

	written, err := os.ReadFile(filepath.Join(root, ".braid", "agents", "reviewer.md"))
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

	providers := map[string]ProviderConfig{
		"fakeprovider": {ID: "fakeprovider", Models: []catwalk.Model{{ID: "fake-model"}}},
	}

	report, err := RunImport(ImportOptions{
		Source: ImportSourceClaude, WorkingDir: root, Agents: true, Providers: providers,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusImported, entry.Status)
	require.Empty(t, entry.Warnings)

	written, err := os.ReadFile(filepath.Join(root, ".braid", "agents", "reviewer.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "model: fakeprovider/fake-model")
}

// Claude Code's tool names are translated to Braid's own.
func TestRunImport_ClaudeAgent_ToolTranslation(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
tools: Read, Grep, Bash
---
You review code.`)

	report, err := RunImport(ImportOptions{
		Source: ImportSourceClaude, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusImported, entry.Status)

	written, err := os.ReadFile(filepath.Join(root, ".braid", "agents", "reviewer.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "tools:")
	require.Contains(t, string(written), "view")
	require.Contains(t, string(written), "grep")
	require.Contains(t, string(written), "bash")
}

// A tool name with no Braid equivalent is dropped and reported, not kept
// verbatim.
func TestRunImport_Agent_UnknownToolDropped(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".claude/agents", "reviewer.md", `---
name: reviewer
description: Reviews code
tools: Read, WebSearch
---
You review code.`)

	report, err := RunImport(ImportOptions{
		Source: ImportSourceClaude, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "reviewer")
	require.Equal(t, StatusAdjusted, entry.Status)
	require.Contains(t, entry.Warnings[0], "WebSearch")

	written, err := os.ReadFile(filepath.Join(root, ".braid", "agents", "reviewer.md"))
	require.NoError(t, err)
	require.NotContains(t, string(written), "WebSearch")
}

// opencode's permission block is not enforced — it must be reported and
// dropped, never silently written into a Braid agent file (there's nowhere
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

	report, err := RunImport(ImportOptions{
		Source: ImportSourceOpenCode, WorkingDir: root, Agents: true,
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

	written, err := os.ReadFile(filepath.Join(root, ".braid", "agents", "dba.md"))
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

	report, err := RunImport(ImportOptions{
		Source: ImportSourceOpenCode, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "agent", "dba")
	require.Equal(t, StatusAdjusted, entry.Status)

	written, err := os.ReadFile(filepath.Join(root, ".braid", "agents", "dba.md"))
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

	report, err := RunImport(ImportOptions{
		Source: ImportSourceClaude, WorkingDir: root, Skills: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "skill", "pdf-fill")
	require.Equal(t, StatusImported, entry.Status)

	written, err := os.ReadFile(filepath.Join(root, ".braid", "skills", "pdf-fill", "SKILL.md"))
	require.NoError(t, err)
	require.Contains(t, string(written), "Fill the form.")
}

// A skill whose name doesn't match its directory fails Braid's own
// validation and is skipped with a reason, not partially written.
func TestRunImport_Skill_InvalidIsSkipped(t *testing.T) {
	root := t.TempDir()
	writeForeignSkill(t, root, ".claude/skills", "pdf-fill", `---
name: totally-different-name
description: Fill PDF forms.
---
Fill the form.`)

	report, err := RunImport(ImportOptions{
		Source: ImportSourceClaude, WorkingDir: root, Skills: true,
	})
	require.NoError(t, err)

	entry := findEntry(t, report, "skill", "pdf-fill")
	require.Equal(t, StatusSkipped, entry.Status)
	require.NotEmpty(t, entry.Reason)

	_, err = os.Stat(filepath.Join(root, ".braid", "skills", "totally-different-name"))
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

	report, err := RunImport(ImportOptions{
		Source: ImportSourceClaude, WorkingDir: root, Agents: true, Skills: true, DryRun: true,
	})
	require.NoError(t, err)

	require.Equal(t, StatusImported, findEntry(t, report, "agent", "reviewer").Status)
	require.Equal(t, StatusImported, findEntry(t, report, "skill", "pdf-fill").Status)

	_, err = os.Stat(filepath.Join(root, ".braid", "agents", "reviewer.md"))
	require.True(t, os.IsNotExist(err), "dry-run must not write the agent file")
	_, err = os.Stat(filepath.Join(root, ".braid", "skills", "pdf-fill"))
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

	opts := ImportOptions{Source: ImportSourceClaude, WorkingDir: root, Agents: true}

	_, err := RunImport(opts)
	require.NoError(t, err)

	dest := filepath.Join(root, ".braid", "agents", "reviewer.md")
	require.NoError(t, os.WriteFile(dest, []byte("hand-edited"), 0o644))

	report, err := RunImport(opts)
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

	opts := ImportOptions{Source: ImportSourceClaude, WorkingDir: root, Agents: true}
	_, err := RunImport(opts)
	require.NoError(t, err)

	dest := filepath.Join(root, ".braid", "agents", "reviewer.md")
	require.NoError(t, os.WriteFile(dest, []byte("hand-edited"), 0o644))

	opts.Force = true
	report, err := RunImport(opts)
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

	report, err := RunImport(ImportOptions{
		Source: ImportSourceOpenCode, WorkingDir: root, Agents: true, Skills: true,
	})
	require.NoError(t, err)
	require.Empty(t, report.Entries)
}

// opencode's disabled/primary agents are skipped, same as regular
// discovery does for .braid/agents.
func TestRunImport_OpenCodeAgent_SkipsPrimaryAndDisabled(t *testing.T) {
	root := t.TempDir()
	writeForeignAgent(t, root, ".opencode/agent", "build.md", "---\nmode: primary\ndescription: x\n---\nbody")
	writeForeignAgent(t, root, ".opencode/agent", "off.md", "---\nname: off\ndisabled: true\ndescription: x\n---\nbody")

	report, err := RunImport(ImportOptions{
		Source: ImportSourceOpenCode, WorkingDir: root, Agents: true,
	})
	require.NoError(t, err)

	require.Equal(t, StatusSkipped, findEntry(t, report, "agent", "build").Status)
	require.Equal(t, StatusSkipped, findEntry(t, report, "agent", "off").Status)
}

func TestRunImport_RejectsUnknownSource(t *testing.T) {
	_, err := RunImport(ImportOptions{Source: "cursor", WorkingDir: t.TempDir(), Agents: true})
	require.Error(t, err)
}

func TestRunImport_RequiresSkillsOrAgents(t *testing.T) {
	_, err := RunImport(ImportOptions{Source: ImportSourceClaude, WorkingDir: t.TempDir()})
	require.Error(t, err)
}
