package config

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/skills"
	"gopkg.in/yaml.v3"
)

// ImportSource identifies which foreign tool's files `sennit import` reads.
// Auto-discovery only ever looks at .sennit/skills and .sennit/agents (see
// load.go, agents_markdown.go); this is the only supported path for bringing
// in a Claude Code or opencode role or skill, and it goes through validation
// and an explicit report rather than trusting a foreign directory.
type ImportSource string

const (
	ImportSourceClaude   ImportSource = "claude"
	ImportSourceOpenCode ImportSource = "opencode"
)

// ImportOptions configures a sennit import run.
type ImportOptions struct {
	Source     ImportSource
	WorkingDir string
	Providers  map[string]ProviderConfig
	Skills     bool // import skills
	Agents     bool // import agents
	DryRun     bool // report what would happen, write nothing
	Global     bool // read/write the user-level directories instead of the project ones
	Force      bool // overwrite files that already exist at the destination
}

// ImportStatus is the outcome recorded for one imported item.
type ImportStatus string

const (
	// StatusImported means the item was written (or would be, under
	// --dry-run) without needing any adjustment.
	StatusImported ImportStatus = "imported"
	// StatusAdjusted means the item was written, but with warnings — a
	// field was dropped, mapped to the nearest equivalent, or left as a
	// frontmatter comment because Sennit has no matching field.
	StatusAdjusted ImportStatus = "adjusted"
	// StatusSkipped means nothing was written; Reason explains why.
	StatusSkipped ImportStatus = "skipped"
)

// ImportEntry reports the outcome for a single skill or agent.
type ImportEntry struct {
	Kind     string // "skill" or "agent"
	Name     string
	Status   ImportStatus
	Reason   string   // set when Status is StatusSkipped
	Warnings []string // set when Status is StatusAdjusted
	Dest     string   // destination path, populated even under --dry-run
}

// ImportReport is the result of a sennit import run.
type ImportReport struct {
	Source  ImportSource
	Entries []ImportEntry
	// Searched lists every source directory that was looked in, for the
	// kinds this run asked for, whether or not it existed. It is what
	// lets "nothing to import" be distinguished from "looked in the
	// wrong place" without the user having to guess which directories
	// this build believes in.
	Searched []string
}

// RunImport copies skills and/or agents from a foreign tool's directories
// into Sennit's own (.sennit/skills, .sennit/agents, or their global
// equivalents under --global), validating and translating as it goes. It
// never touches source files. Nothing is written when opts.DryRun is set;
// callers get the same report either way.
func RunImport(opts ImportOptions) (ImportReport, error) {
	if opts.Source != ImportSourceClaude && opts.Source != ImportSourceOpenCode {
		return ImportReport{}, fmt.Errorf("unknown import source %q (want %q or %q)", opts.Source, ImportSourceClaude, ImportSourceOpenCode)
	}
	if !opts.Skills && !opts.Agents {
		return ImportReport{}, errors.New("nothing to import: pass --skills and/or --agents")
	}

	srcSkillsDirs, srcAgentsDirs := importSourceDirs(opts.Source, opts.WorkingDir, opts.Global)
	dstSkillsDir, dstAgentsDir := importDestDirs(opts.WorkingDir, opts.Global)

	report := ImportReport{Source: opts.Source}

	// seen tracks which source directory each name was taken from, so a
	// tool that reads two spellings of the same directory does not import
	// the same skill twice. Directories are visited most-canonical first,
	// so the first one to claim a name keeps it.
	if opts.Skills {
		seen := map[string]string{}
		for _, srcDir := range srcSkillsDirs {
			report.Searched = append(report.Searched, srcDir)
			entries, err := importSkills(srcDir, dstSkillsDir, opts, seen)
			if err != nil {
				return report, err
			}
			report.Entries = append(report.Entries, entries...)
		}
	}
	if opts.Agents {
		seen := map[string]string{}
		for _, srcDir := range srcAgentsDirs {
			report.Searched = append(report.Searched, srcDir)
			entries, err := importAgents(srcDir, dstAgentsDir, opts, seen)
			if err != nil {
				return report, err
			}
			report.Entries = append(report.Entries, entries...)
		}
	}

	sort.SliceStable(report.Entries, func(i, j int) bool {
		if report.Entries[i].Kind != report.Entries[j].Kind {
			return report.Entries[i].Kind < report.Entries[j].Kind
		}
		return report.Entries[i].Name < report.Entries[j].Name
	})

	return report, nil
}

// importSourceDirs returns the directories a foreign tool keeps skills and
// agents in, most-canonical first. More than one per kind is not
// hedging: opencode genuinely reads both spellings, so importing only one
// would quietly miss half its users.
//
// The Claude Code forms mirror the directories Sennit itself used to
// auto-discover before imports became explicit (see git history of
// agentDirs and projectSkillSubdirs).
//
// The opencode forms are verified against a real installation (v1.18.18)
// rather than inferred: it registers <dir>/skill and <dir>/skills as
// skill sources and reads both <dir>/agent and <dir>/agents for agents,
// for every directory in its config chain — project-local .opencode and
// the global one alike. Its global root is $XDG_CONFIG_HOME/opencode
// falling back to ~/.config/opencode, which is exactly what home.Config
// resolves.
func importSourceDirs(source ImportSource, workingDir string, global bool) (skillsDirs, agentsDirs []string) {
	join := func(root string, names ...string) []string {
		out := make([]string, 0, len(names))
		for _, name := range names {
			out = append(out, filepath.Join(root, name))
		}
		return out
	}

	switch source {
	case ImportSourceClaude:
		root := filepath.Join(workingDir, ".claude")
		if global {
			root = filepath.Join(home.Dir(), ".claude")
		}
		return join(root, "skills"), join(root, "agents")
	case ImportSourceOpenCode:
		root := filepath.Join(workingDir, ".opencode")
		if global {
			root = filepath.Join(home.Config(), "opencode")
		}
		return join(root, "skills", "skill"), join(root, "agent", "agents")
	default:
		return nil, nil
	}
}

// importDestDirs returns Sennit's own skill/agent directories — the only
// ones discovery reads, project or global depending on opts.Global.
func importDestDirs(workingDir string, global bool) (skillsDir, agentsDir string) {
	if global {
		return filepath.Join(home.Config(), appName, "skills"), filepath.Join(filepath.Dir(GlobalConfig()), "agents")
	}
	return filepath.Join(workingDir, brand.DataDir, "skills"), filepath.Join(workingDir, brand.DataDir, "agents")
}

// importSkills copies every <srcDir>/<name>/SKILL.md directory that parses
// and validates as a Sennit-compatible skill into dstDir. A missing srcDir is
// not an error — it just means there is nothing of that kind to import.
//
// seen carries names already taken by an earlier source directory (see
// RunImport) and is extended with the ones this call claims. Matching on
// the directory name rather than the parsed skill name is deliberate: the
// spec ties the two together, and a name has to be claimed before the
// file is parsed for a --dry-run to report the same outcome as a real
// run.
func importSkills(srcDir, dstDir string, opts ImportOptions, seen map[string]string) ([]ImportEntry, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", srcDir, err)
	}

	var out []ImportEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if from, dup := seen[e.Name()]; dup {
			out = append(out, ImportEntry{
				Kind: "skill", Name: e.Name(), Status: StatusSkipped,
				Reason: fmt.Sprintf("already imported from %s", from),
			})
			continue
		}
		seen[e.Name()] = srcDir
		skillDir := filepath.Join(srcDir, e.Name())
		skillFile := filepath.Join(skillDir, skills.SkillFileName)

		skill, err := skills.Parse(skillFile)
		if err != nil {
			out = append(out, ImportEntry{
				Kind: "skill", Name: e.Name(), Status: StatusSkipped,
				Reason: fmt.Sprintf("cannot parse %s: %v", skills.SkillFileName, err),
			})
			continue
		}
		// Sennit enforces the same Agent Skills spec these tools' skills
		// already follow, so this mostly catches name/directory mismatches
		// and oversized descriptions rather than anything tool-specific.
		if err := skill.Validate(); err != nil {
			out = append(out, ImportEntry{
				Kind: "skill", Name: e.Name(), Status: StatusSkipped,
				Reason: fmt.Sprintf("not sennit-compatible: %v", err),
			})
			continue
		}

		dest := filepath.Join(dstDir, skill.Name)
		entry := ImportEntry{Kind: "skill", Name: skill.Name, Status: StatusImported, Dest: dest}

		if _, err := os.Stat(dest); err == nil {
			if !opts.Force {
				entry.Status = StatusSkipped
				entry.Reason = "already exists (use --force to overwrite)"
				out = append(out, entry)
				continue
			}
		}

		if !opts.DryRun {
			if err := copyDir(skillDir, dest); err != nil {
				entry.Status = StatusSkipped
				entry.Reason = fmt.Sprintf("copy failed: %v", err)
				out = append(out, entry)
				continue
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// copyDir recursively copies src onto dst, preserving any files alongside
// SKILL.md (reference docs, scripts, ...) that a skill directory may carry.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
}

// importAgentMeta is the frontmatter of a foreign agent file, extended with
// fields regular discovery (markdownAgent) never needs to know about:
// opencode's "effort" alias, its "temperature"/"top_p" model tuning, and its
// permission block. All three only matter here, for reporting/translation.
type importAgentMeta struct {
	Name            string     `yaml:"name"`
	Description     string     `yaml:"description"`
	Model           string     `yaml:"model"`
	ReasoningEffort string     `yaml:"reasoning_effort"`
	Effort          string     `yaml:"effort"`
	Temperature     *float64   `yaml:"temperature"`
	TopP            *float64   `yaml:"top_p"`
	Tools           stringList `yaml:"tools"`
	Mode            string     `yaml:"mode"`
	Disabled        bool       `yaml:"disabled"`
	Permission      yaml.Node  `yaml:"permission"`
}

// outAgentFrontmatter is the frontmatter Sennit actually writes. A dedicated
// (rather than reused) type keeps yaml.Marshal from ever emitting a field
// Sennit's own agent files don't understand.
type outAgentFrontmatter struct {
	Name            string   `yaml:"name"`
	Description     string   `yaml:"description,omitempty"`
	Model           string   `yaml:"model,omitempty"`
	ReasoningEffort string   `yaml:"reasoning_effort,omitempty"`
	Tools           []string `yaml:"tools,omitempty"`
}

// importAgents converts every *.md file in srcDir into a Sennit agent file
// under dstDir. A missing srcDir is not an error — nothing to import.
func importAgents(srcDir, dstDir string, opts ImportOptions, seen map[string]string) ([]ImportEntry, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", srcDir, err)
	}

	var out []ImportEntry
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		// Filename, not the frontmatter id: the id is only known after
		// conversion, and by then a duplicate would already have been
		// written. See importSkills for the same reasoning.
		if from, dup := seen[e.Name()]; dup {
			out = append(out, ImportEntry{
				Kind: "agent", Name: e.Name(), Status: StatusSkipped,
				Reason: fmt.Sprintf("already imported from %s", from),
			})
			continue
		}
		seen[e.Name()] = srcDir
		entry, err := convertAgentFile(filepath.Join(srcDir, e.Name()), e.Name(), dstDir, opts)
		if err != nil {
			out = append(out, ImportEntry{Kind: "agent", Name: e.Name(), Status: StatusSkipped, Reason: err.Error()})
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// convertAgentFile parses one foreign agent file, translates what it can,
// and (unless opts.DryRun) writes the result under dstDir. A field that
// can't be translated is dropped from the written frontmatter, recorded as
// a warning, and left behind as a "# original ..." comment so the user can
// see what was lost without diffing against the source file.
func convertAgentFile(path, filename, dstDir string, opts ImportOptions) (ImportEntry, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return ImportEntry{}, err
	}

	frontmatter, body, err := splitAgentFrontmatter(string(content))
	if err != nil {
		return ImportEntry{}, fmt.Errorf("no valid frontmatter: %w", err)
	}

	var meta importAgentMeta
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return ImportEntry{}, fmt.Errorf("parsing frontmatter: %w", err)
	}

	id := meta.Name
	if id == "" {
		id = strings.TrimSuffix(filename, filepath.Ext(filename))
	}

	if meta.Disabled {
		return ImportEntry{Kind: "agent", Name: id, Status: StatusSkipped, Reason: "disabled in source file"}, nil
	}
	if strings.EqualFold(meta.Mode, "primary") {
		return ImportEntry{Kind: "agent", Name: id, Status: StatusSkipped, Reason: "opencode primary-mode agents are driven directly, not delegated to"}, nil
	}
	if !validAgentID(id) {
		return ImportEntry{Kind: "agent", Name: id, Status: StatusSkipped, Reason: "invalid agent name (letters, digits, '_' or '-' only)"}, nil
	}
	prompt := strings.TrimSpace(body)
	if prompt == "" {
		return ImportEntry{Kind: "agent", Name: id, Status: StatusSkipped, Reason: "empty body: no system prompt"}, nil
	}

	var warnings []string
	var comments []string
	fm := outAgentFrontmatter{Name: id, Description: meta.Description}

	if meta.Model != "" {
		if match, err := ResolveModelString(opts.Providers, meta.Model); err == nil {
			fm.Model = match.Provider + "/" + match.ModelID
		} else {
			warnings = append(warnings, fmt.Sprintf("model %q not available; agent will inherit the main model", meta.Model))
			comments = append(comments, fmt.Sprintf("# original model: %s — not available", meta.Model))
		}
	}

	if effort := cmp.Or(meta.ReasoningEffort, meta.Effort); effort != "" {
		mapped, adjusted, ok := normalizeReasoningEffort(effort)
		switch {
		case !ok:
			warnings = append(warnings, fmt.Sprintf("reasoning_effort %q not recognized; dropped", effort))
			comments = append(comments, fmt.Sprintf("# original reasoning_effort: %s — not recognized", effort))
		case adjusted:
			warnings = append(warnings, fmt.Sprintf("reasoning_effort %q mapped to %q", effort, mapped))
			fm.ReasoningEffort = mapped
		default:
			fm.ReasoningEffort = mapped
		}
	}

	if meta.Tools != nil {
		mapped, dropped := translateAgentTools([]string(meta.Tools))
		fm.Tools = mapped
		for _, d := range dropped {
			warnings = append(warnings, fmt.Sprintf("tool %q has no Sennit equivalent; dropped", d))
		}
	}

	// Sennit agents have no temperature/top_p field (see config.Agent); the
	// value is preserved as a comment rather than silently lost.
	if meta.Temperature != nil {
		warnings = append(warnings, "temperature is not supported by sennit agents")
		comments = append(comments, fmt.Sprintf("# original temperature: %v (not supported by sennit agents)", *meta.Temperature))
	}
	if meta.TopP != nil {
		warnings = append(warnings, "top_p is not supported by sennit agents")
		comments = append(comments, fmt.Sprintf("# original top_p: %v (not supported by sennit agents)", *meta.TopP))
	}

	// See TECHDEBT.md, "Limitations of imported agent definitions":
	// permission blocks were never enforced even when the source directory
	// was auto-discovered, and that hasn't changed for import.
	if !meta.Permission.IsZero() {
		warnings = append(warnings, "permission block is not supported; restrict tools via the tools list instead")
		comments = append(comments, "# original permission block dropped: not supported, use tools list")
	}

	yamlBytes, err := yaml.Marshal(&fm)
	if err != nil {
		return ImportEntry{}, fmt.Errorf("encoding frontmatter: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("---\n")
	for _, c := range comments {
		sb.WriteString(c)
		sb.WriteString("\n")
	}
	sb.Write(yamlBytes)
	sb.WriteString("---\n\n")
	sb.WriteString(prompt)
	sb.WriteString("\n")

	dest := filepath.Join(dstDir, id+".md")
	status := StatusImported
	if len(warnings) > 0 {
		status = StatusAdjusted
	}
	entry := ImportEntry{Kind: "agent", Name: id, Status: status, Warnings: warnings, Dest: dest}

	if _, err := os.Stat(dest); err == nil && !opts.Force {
		entry.Status = StatusSkipped
		entry.Reason = "already exists (use --force to overwrite)"
		entry.Warnings = nil
		return entry, nil
	}

	if !opts.DryRun {
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return entry, err
		}
		if err := os.WriteFile(dest, []byte(sb.String()), 0o644); err != nil {
			return entry, err
		}
	}

	return entry, nil
}

// normalizeReasoningEffort maps a foreign effort value onto Sennit's
// low/medium/high scale. ok is false when the value can't be mapped at all
// (the caller drops it); adjusted is true when the mapping isn't an exact
// match (the caller still uses it, but warns).
func normalizeReasoningEffort(v string) (mapped string, adjusted bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(v)), false, true
	case "minimal", "min", "none":
		return "low", true, true
	case "max", "maximum", "xhigh", "ultra":
		return "high", true, true
	default:
		return "", false, false
	}
}

// importKnownTools are Sennit's own tool names, accepted as pass-through
// during import. Kept as a literal set rather than importing
// internal/agent/tools for its name constants: that package already imports
// internal/config, and config importing it back would cycle.
var importKnownTools = map[string]bool{
	"read": true, "write": true, "edit": true, "multiedit": true, "bash": true,
	"grep": true, "ripgrep": true, "glob": true, "ls": true, "fetch": true, "web_fetch": true,
	"web_search": true, "download": true, "todos": true, "agent": true,
	"question": true, brand.ToolInfo: true, brand.ToolLogs: true,
	"job_output": true, "job_kill": true,
	"thread_create": true, "thread_list": true, "thread_merge": true,
	"thread_remove": true, "thread_send": true, "thread_status": true, "thread_wait": true,
	"task_list": true, "task_result": true, "task_cancel": true,
	"task_send": true, "task_output": true,
	"lsp_definition": true, "lsp_references": true, "lsp_rename": true,
	"lsp_symbols": true, "lsp_call_hierarchy": true, "lsp_diagnostics": true,
	"lsp_restart": true, "lsp_replace_symbol": true,
	"list_mcp_resources": true, "read_mcp_resource": true,
}

// translateAgentTools maps foreign tool names onto Sennit's, the same way
// normalizeToolNames does for .sennit/agents files — except here a name that
// resolves to neither a known Claude Code name nor a known Sennit name is
// reported back as dropped, rather than kept as-is: an imported file that
// silently granted the wrong tools would be worse than one that grants
// fewer, correctly.
func translateAgentTools(names []string) (mapped []string, dropped []string) {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		switch {
		case ClaudeToolNames[lower] != "":
			name = ClaudeToolNames[lower]
		case importKnownTools[lower]:
			name = lower
		case legacyToolNames[lower] != "":
			// One of Sennit's own older names — fold it forward rather
			// than dropping it. See [CanonicalToolName].
			name = legacyToolNames[lower]
		default:
			dropped = append(dropped, name)
			continue
		}
		if !seen[name] {
			seen[name] = true
			mapped = append(mapped, name)
		}
	}
	return mapped, dropped
}
