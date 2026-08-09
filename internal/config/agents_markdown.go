package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// agentDirs are scanned for `*.md` agent definitions, lowest priority first,
// so a later directory overrides an agent of the same name from an earlier one.
//
// The Claude and opencode directories are read on purpose: their agent files
// use the same frontmatter-plus-body shape, and a project that already defines
// roles for those tools should not have to restate them here.
var agentDirs = []string{
	filepath.Join(".opencode", "agent"),
	filepath.Join(".claude", "agents"),
	filepath.Join(".braid", "agents"),
}

// markdownAgent is the frontmatter of an agent file. Fields absent from a
// foreign tool's format simply stay zero and fall back to Braid's defaults.
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
	Tools stringList `yaml:"tools"`
	// Mode is opencode's field; "primary" marks an agent meant to be driven
	// directly rather than delegated to, so those files are skipped.
	Mode     string `yaml:"mode"`
	Disabled bool   `yaml:"disabled"`
}

// stringList accepts both `tools: [a, b]` and `tools: a, b`.
type stringList []string

func (s *stringList) UnmarshalYAML(value *yaml.Node) error {
	var list []string
	if err := value.Decode(&list); err == nil {
		*s = list
		return nil
	}

	var joined string
	if err := value.Decode(&joined); err != nil {
		return errors.New("tools must be a list or a comma-separated string")
	}
	for _, part := range strings.Split(joined, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// claudeToolNames maps Claude Code's tool names onto Braid's. Without this a
// `tools: Read, Grep` line would grant nothing at all, since the allow-list is
// matched against Braid's own tool names.
var claudeToolNames = map[string]string{
	"read":      "view",
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

	frontmatter, body, err := splitAgentFrontmatter(string(content))
	if err != nil {
		return "", Agent{}, err
	}

	var meta markdownAgent
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
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
	if !validAgentID(id) {
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

// normalizeToolNames maps foreign tool names onto Braid's and drops duplicates.
// Names it does not know are kept as-is so a Braid-native file can list Braid
// tools directly.
func normalizeToolNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if mapped, ok := claudeToolNames[strings.ToLower(name)]; ok {
			name = mapped
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// splitAgentFrontmatter separates the leading YAML block from the body. It
// mirrors the skills parser rather than importing it, so the config package
// keeps its current dependency set.
func splitAgentFrontmatter(content string) (frontmatter, body string, err error) {
	content = strings.TrimPrefix(content, "\uFEFF")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	lines := strings.Split(content, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.TrimSpace(line) != ""
	})
	if start == -1 || strings.TrimSpace(lines[start]) != "---" {
		return "", "", errors.New("no YAML frontmatter found")
	}

	endOffset := slices.IndexFunc(lines[start+1:], func(line string) bool {
		return strings.TrimSpace(line) == "---"
	})
	if endOffset == -1 {
		return "", "", errors.New("unclosed frontmatter")
	}
	end := start + 1 + endOffset

	return strings.Join(lines[start+1:end], "\n"), strings.Join(lines[end+1:], "\n"), nil
}
