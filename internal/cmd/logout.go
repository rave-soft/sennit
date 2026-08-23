package cmd

import (
	"fmt"

	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/spf13/cobra"
)

var (
	logoutHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	logoutItemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	logoutPromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("215"))
)

var logoutCmd = &cobra.Command{
	Aliases: []string{"signout"},
	Use:     "logout [platform]",
	Short:   "Logout Sennit from a platform",
	Long: `Logout Sennit from a specified platform, removing stored credentials.
The platform should be provided as an argument.
If no argument is given, a list of logged-in platforms will be shown.
Available platforms are: copilot, codex.`,
	Example: `
# Sign out from GitHub Copilot
sennit logout copilot

# Sign out from OpenAI Codex
sennit logout codex
  `,
	ValidArgs: []cobra.Completion{
		"copilot",
		"github",
		"github-copilot",
		"codex",
		"chatgpt",
		"openai-codex",
	},
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		var provider string
		if len(args) == 0 {
			provider = pickLoggedInProvider(ws)
			if provider == "" {
				return nil
			}
		} else {
			provider = args[0]
		}

		force, _ := cmd.Flags().GetBool("force")
		if !force {
			fmt.Print(logoutPromptStyle.Render(fmt.Sprintf("Are you sure you want to logout %s? (y/N) ", provider)))
			var response string
			_, err := fmt.Scanln(&response)
			if err != nil || (response != "y" && response != "Y" && response != "yes" && response != "Yes" && response != "YES") {
				fmt.Println(logoutHeaderStyle.Render("Logout cancelled."))
				return nil
			}
		}

		switch provider {
		case "hyper":
			return logoutHyper(ws)
		case "copilot", "github", "github-copilot":
			return logoutCopilot(ws)
		case "codex", "chatgpt", "openai-codex":
			return logoutCodex(ws)
		default:
			return fmt.Errorf("unknown platform: %s", provider)
		}
	},
}

// logoutProvider removes providerID's stored credentials (api_key and oauth,
// plus any extraFields such as Codex's per-account model list) and prints
// the standard confirmation line.
//
// Every field is removed unconditionally rather than short-circuited on the
// first error, and the first failure, if any, is what gets returned. This
// used to be spelled with cmp.Or() over the calls, but staticcheck's SA4023
// (newer analyzer versions; see .golangci.yml note) misreads cmp.Or's
// generic instantiation over an interface return type and claims the
// resulting `err != nil` is always true — it is not, RemoveConfigField
// returns nil on the common success path, confirmed by a minimal cmp.Or
// repro outside this codebase. Spelling it out avoids the false positive
// without weakening the check.
func logoutProvider(ws workspace.ConfigAccessor, providerID, displayName string, extraFields ...string) error {
	fields := append([]string{
		"providers." + providerID + ".api_key",
		"providers." + providerID + ".oauth",
	}, extraFields...)

	var firstErr error
	for _, field := range fields {
		if err := ws.RemoveConfigField(config.ScopeGlobal, field); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}

	fmt.Println(logoutHeaderStyle.Render(fmt.Sprintf("Successfully logged out of %s.", displayName)))
	return nil
}

func logoutHyper(ws workspace.ConfigAccessor) error {
	return logoutProvider(ws, "hyper", "Hyper")
}

func logoutCopilot(ws workspace.ConfigAccessor) error {
	return logoutProvider(ws, "copilot", "GitHub Copilot")
}

// logoutCodex drops the Codex credentials. The discovered model list goes
// with them: it is per-account, so leaving it behind would advertise models
// the next account may not have.
func logoutCodex(ws workspace.ConfigAccessor) error {
	return logoutProvider(ws, "codex", "OpenAI Codex", "providers.codex.models")
}

func pickLoggedInProvider(ws workspace.ConfigAccessor) string {
	cfg := ws.Config()
	if cfg == nil {
		fmt.Println(logoutPromptStyle.Render("You are not logged in to any platform."))
		return ""
	}

	type loggedInProvider struct {
		id   string
		name string
	}

	// Only OAuth-based providers support login/logout. Keep this list in sync
	// with the switch in RunE and the login command.
	oauthProviders := map[string]string{
		"hyper":   "Hyper",
		"copilot": "GitHub Copilot",
		"codex":   "OpenAI Codex",
	}

	var loggedIn []loggedInProvider
	for id, name := range oauthProviders {
		if p, ok := cfg.Providers.Get(id); ok && p.OAuthToken != nil {
			loggedIn = append(loggedIn, loggedInProvider{id: id, name: name})
		}
	}

	if len(loggedIn) == 0 {
		fmt.Println(logoutPromptStyle.Render("You are not logged in to any platform."))
		return ""
	}

	if len(loggedIn) == 1 {
		return loggedIn[0].id
	}

	fmt.Println(logoutHeaderStyle.Render("Logged-in platforms:"))
	for i, p := range loggedIn {
		fmt.Println(logoutItemStyle.Render(fmt.Sprintf("  %d. %s", i+1, p.name)))
	}
	fmt.Print(logoutPromptStyle.Render(fmt.Sprintf("Select a platform to logout (1-%d): ", len(loggedIn))))

	var choice int
	_, err := fmt.Scanln(&choice)
	if err != nil || choice < 1 || choice > len(loggedIn) {
		fmt.Println(logoutHeaderStyle.Render("Logout cancelled."))
		return ""
	}

	return loggedIn[choice-1].id
}

func init() {
	logoutCmd.Flags().BoolP("force", "f", false, "Skip logout confirmation prompt")
}
