package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/frontmatter"
	"gopkg.in/yaml.v3"
)

// agentDirs are scanned for `*.md` agent definitions, lowest priority first,
// so a later directory overrides an agent of the same name from an earlier one.
//
// Only Sennit's own directory is scanned. Agent files written for other tools
// (Claude Code's .claude/agents, opencode's .opencode/agent) are not
// auto-discovered — `sennit import` copies and validates them into
// .sennit/agents instead of trusting a foreign directory implicitly.
var agentDirs = []string{
	filepath.Join(brand.DataDir, "agents"),
}

// markdownAgent is the frontmatter of an agent file. Fields absent from a
// foreign tool's format simply stay zero and fall back to Sennit's defaults.
type markdownAgent struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Model is a "provider/model-id" string. Foreign files name a concrete
	// provider model here ("opus", "some/provider/model"); it is honoured if
	// it resolves against a configured provider, and otherwise falls back to
	// the default (the app's main model).
	Model string `yaml:"model"`
	// ReasoningEffort overrides the model's effort for this agent.
	ReasoningEffort string `yaml:"reasoning_effort"`
	// Tools restricts the agent's tools. Accepts a YAML list or the
	// comma-separated string Claude Code uses.
	Tools StringList `yaml:"tools"`
	// Mode is opencode's field; "primary" marks an agent meant to be driven
	// directly rather than delegated to, so those files are skipped.
	Mode     string `yaml:"mode"`
	Disabled bool   `yaml:"disabled"`
}

// StringList accepts `tools: [a, b]`, the comma-separated string Claude Code
// uses (`tools: a, b`), and the enabled-map form opencode uses
// (`tools: {a: true, b: false}`) — only keys mapped to true are kept.
//
// Exported for internal/importer, which must accept exactly what loading an
// agent file accepts: the point of `sennit import` is that what it writes
// loads, so the two have to agree on the shape rather than each have their
// own idea of it.
type StringList []string

func (s *StringList) UnmarshalYAML(value *yaml.Node) error {
	var list []string
	if err := value.Decode(&list); err == nil {
		*s = list
		return nil
	}

	var enabled map[string]bool
	if err := value.Decode(&enabled); err == nil {
		keys := make([]string, 0, len(enabled))
		for name, on := range enabled {
			if on {
				keys = append(keys, name)
			}
		}
		slices.Sort(keys) // map iteration order is random; keep output stable.
		*s = keys
		return nil
	}

	var joined string
	if err := value.Decode(&joined); err != nil {
		return errors.New("tools must be a list, a comma-separated string, or a name-to-bool map")
	}
	for _, part := range strings.Split(joined, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// ClaudeToolNames maps Claude Code's tool names onto Sennit's. It is exported
// for `sennit import` (see import.go), which is now the only place that
// translates foreign tool names — regular discovery only reads
// .sennit/agents, whose files are expected to already name Sennit's own tools.
var ClaudeToolNames = map[string]string{
	"read":      "read",
	"write":     "write",
	"edit":      "edit",
	"bash":      "bash",
	"grep":      "grep",
	"glob":      "glob",
	"ls":        "ls",
	"webfetch":  "fetch",
	"todowrite": "todos",
	"task":      "agent",
}

// discoverMarkdownAgents reads agent definitions from every known directory.
// A file that cannot be parsed is reported and skipped: one broken agent must
// not stop the rest from loading.
func discoverMarkdownAgents(workingDir string, providers map[string]ProviderConfig) map[string]Agent {
	found := make(map[string]Agent)
	if workingDir == "" {
		return found
	}

	dirs := make([]string, 0, len(agentDirs)+1)
	for _, dir := range agentDirs {
		dirs = append(dirs, filepath.Join(workingDir, dir))
	}
	// The user-level directory has the last word, matching how the rest of the
	// config treats global settings.
	dirs = append(dirs, filepath.Join(filepath.Dir(GlobalConfig()), "agents"))

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // A missing agents directory is the normal case.
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			id, agent, err := parseAgentFile(path, providers)
			if err != nil {
				slog.Warn("Skipping agent file", "path", path, "error", err)
				continue
			}
			if id == "" {
				continue // Deliberately skipped, e.g. opencode's primary mode.
			}
			found[id] = agent
		}
	}
	return found
}

// parseAgentFile turns one markdown file into an agent. An empty id means the
// file was valid but intentionally not registered.
func parseAgentFile(path string, providers map[string]ProviderConfig) (string, Agent, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", Agent{}, err
	}

	header, body, err := frontmatter.Split(string(content))
	if err != nil {
		return "", Agent{}, err
	}

	var meta markdownAgent
	if err := yaml.Unmarshal([]byte(header), &meta); err != nil {
		return "", Agent{}, err
	}

	if meta.Disabled || strings.EqualFold(meta.Mode, "primary") {
		return "", Agent{}, nil
	}

	// Claude files carry an explicit name; opencode files do not, so the
	// filename is the fallback — it is what the user types either way.
	id := meta.Name
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if !ValidAgentID(id) {
		return "", Agent{}, errors.New("agent name must contain only letters, digits, '_' or '-'")
	}

	prompt := strings.TrimSpace(body)
	if prompt == "" {
		return "", Agent{}, errors.New("agent file has an empty body, which would leave it without a system prompt")
	}

	agent := Agent{
		ID:              id,
		Name:            id,
		Description:     meta.Description,
		Prompt:          prompt,
		ReasoningEffort: meta.ReasoningEffort,
	}

	switch meta.Model {
	case "":
		// Leave unset; empty means inherit the app's main model.
	default:
		// A foreign model reference. It's honoured if it resolves to a
		// configured provider/model; otherwise ignoring it beats failing the
		// file, but say so: the agent silently runs on a different model
		// than its original tool used.
		if match, err := ResolveModelString(providers, meta.Model); err == nil {
			agent.Model = match.Provider + "/" + match.ModelID
		} else {
			slog.Debug("Ignoring unrecognised model in agent file",
				"path", path, "model", meta.Model, "hint", "use \"provider/model-id\"")
		}
	}

	if meta.Tools != nil {
		agent.AllowedTools = normalizeToolNames(meta.Tools)
	}

	return id, agent, nil
}

// normalizeToolNames trims and drops duplicate tool names, folding names
// Sennit has since renamed onto their current ones. Unlike the importer
// (see import.go), it does not translate foreign tool names: .sennit/agents
// is Sennit's own directory, so its files are expected to already name
// Sennit's tools directly — only Sennit's own older names are accepted.
func normalizeToolNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = CanonicalToolName(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}
