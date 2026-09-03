package commands

import (
	"context"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/skills"
)

var namedArgPattern = regexp.MustCompile(`\$([A-Z][A-Z0-9_]*)`)

const (
	userCommandPrefix    = "user:"
	projectCommandPrefix = "project:"
)

// Argument represents a command argument with its metadata.
type Argument struct {
	ID          string
	Title       string
	Description string
	Required    bool
}

// MCPPrompt represents a custom command loaded from an MCP server.
type MCPPrompt struct {
	ID          string
	Title       string
	Description string
	PromptID    string
	ClientID    string
	Arguments   []Argument
}

// CustomCommand represents a user-defined custom command loaded from markdown files.
type CustomCommand struct {
	ID        string
	Name      string
	Content   string
	Arguments []Argument
	// Skill is set when this command represents a user-invocable skill
	Skill *skills.Skill
}

type commandSource struct {
	path   string
	prefix string
}

// LoadCustomCommands loads custom commands from multiple sources including
// XDG config directory, home directory, and project directory.
func LoadCustomCommands(cfg *config.Config) ([]CustomCommand, error) {
	return loadAll(buildCommandSources(cfg))
}

// FromSkillCatalog converts user-invocable catalog entries into custom
// command entries for the command palette.
func FromSkillCatalog(entries []skills.CatalogEntry) []CustomCommand {
	commands := make([]CustomCommand, 0, len(entries))
	for _, entry := range entries {
		if !entry.UserInvocable {
			continue
		}
		name := entry.Label
		if name == "" {
			name = userCommandPrefix + entry.Name
		}
		commands = append(commands, CustomCommand{
			ID:   name,
			Name: name,
			Skill: &skills.Skill{
				Name:          entry.Name,
				Description:   entry.Description,
				SkillFilePath: entry.ID,
			},
		})
	}
	return commands
}

// LoadMCPPrompts loads custom commands from an MCP prompt catalog. It takes
// the catalog as a sequence rather than *mcp.Registry itself (its only
// caller, internal/workspace/appws, has one to spare) so this package —
// which otherwise just reads .md files off disk — does not have to pull in
// the MCP client registry, its transport, and process launching.
func LoadMCPPrompts(prompts iter.Seq2[string, []*sdkmcp.Prompt]) ([]MCPPrompt, error) {
	if prompts == nil {
		return nil, nil
	}
	var commands []MCPPrompt
	for mcpName, prompts := range prompts {
		for _, prompt := range prompts {
			key := mcpName + ":" + prompt.Name
			var args []Argument
			for _, arg := range prompt.Arguments {
				title := arg.Title
				if title == "" {
					title = arg.Name
				}
				args = append(args, Argument{
					ID:          arg.Name,
					Title:       title,
					Description: arg.Description,
					Required:    arg.Required,
				})
			}
			commands = append(commands, MCPPrompt{
				ID:          key,
				Title:       prompt.Title,
				Description: prompt.Description,
				PromptID:    prompt.Name,
				ClientID:    mcpName,
				Arguments:   args,
			})
		}
	}
	return commands, nil
}

func buildCommandSources(cfg *config.Config) []commandSource {
	return []commandSource{
		{
			path:   filepath.Join(home.Config(), brand.Slug, "commands"),
			prefix: userCommandPrefix,
		},
		{
			path:   filepath.Join(home.Dir(), brand.DataDir, "commands"),
			prefix: userCommandPrefix,
		},
		{
			path:   filepath.Join(cfg.Options.DataDirectory, "commands"),
			prefix: projectCommandPrefix,
		},
	}
}

func loadAll(sources []commandSource) ([]CustomCommand, error) {
	var commands []CustomCommand

	for _, source := range sources {
		if cmds, err := loadFromSource(source); err == nil {
			commands = append(commands, cmds...)
		}
	}

	return commands, nil
}

func loadFromSource(source commandSource) ([]CustomCommand, error) {
	if _, err := os.Stat(source.path); os.IsNotExist(err) {
		return nil, nil
	}

	var commands []CustomCommand

	err := filepath.WalkDir(source.path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable entry is not a reason to abandon the walk:
			// returning the error aborted it, and loadAll then discarded
			// this source entirely — a single subdirectory the process
			// cannot read hid every command the source had.
			slog.Warn("Skipping unreadable command path", "path", path, "error", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !isMarkdownFile(d.Name()) {
			return nil
		}

		cmd, err := loadCommand(path, source.path, source.prefix)
		if err != nil {
			return nil // Skip invalid files
		}

		commands = append(commands, cmd)
		return nil
	})

	return commands, err
}

func loadCommand(path, baseDir, prefix string) (CustomCommand, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return CustomCommand{}, err
	}

	id := buildCommandID(path, baseDir, prefix)

	return CustomCommand{
		ID:        id,
		Name:      id,
		Content:   string(content),
		Arguments: extractArgNames(string(content)),
	}, nil
}

func extractArgNames(content string) []Argument {
	matches := namedArgPattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var args []Argument

	for _, match := range matches {
		arg := match[1]
		if !seen[arg] {
			seen[arg] = true
			// for normal custom commands, all args are required
			args = append(args, Argument{ID: arg, Title: arg, Required: true})
		}
	}

	return args
}

func buildCommandID(path, baseDir, prefix string) string {
	relPath, _ := filepath.Rel(baseDir, path)
	parts := strings.Split(relPath, string(filepath.Separator))

	// Remove .md extension from last part
	if len(parts) > 0 {
		lastIdx := len(parts) - 1
		parts[lastIdx] = strings.TrimSuffix(parts[lastIdx], filepath.Ext(parts[lastIdx]))
	}

	return prefix + strings.Join(parts, ":")
}

func isMarkdownFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".md")
}

// GetMCPPrompt takes a closure over the registry's GetPromptMessages
// (rather than *mcp.Registry and mcp.ConfigProvider directly) for the same
// reason LoadMCPPrompts takes a sequence: this package only assembles the
// command palette, and the caller — internal/workspace/appws — already
// holds both the registry and the config store GetPromptMessages needs.
func GetMCPPrompt(getPromptMessages func(ctx context.Context, clientID, promptID string, args map[string]string) ([]string, error), clientID, promptID string, args map[string]string) (string, error) {
	// Create a context with timeout since tea.Cmd doesn't support context passing.
	// The MCP client has its own timeout, but this provides an additional safeguard.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := getPromptMessages(ctx, clientID, promptID, args)
	if err != nil {
		return "", err
	}
	return strings.Join(result, " "), nil
}
