package cmd

import (
	"cmp"
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

func logoutHyper(ws workspace.ConfigAccessor) error {
	if err := cmp.Or(
		ws.RemoveConfigField(config.ScopeGlobal, "providers.hyper.api_key"),
		ws.RemoveConfigField(config.ScopeGlobal, "providers.hyper.oauth"),
	); err != nil {
		return err
	}

	fmt.Println(logoutHeaderStyle.Render("Successfully logged out of Hyper."))
	return nil
}

func logoutCopilot(ws workspace.ConfigAccessor) error {
	if err := cmp.Or(
		ws.RemoveConfigField(config.ScopeGlobal, "providers.copilot.api_key"),
		ws.RemoveConfigField(config.ScopeGlobal, "providers.copilot.oauth"),
	); err != nil {
		return err
	}

	fmt.Println(logoutHeaderStyle.Render("Successfully logged out of GitHub Copilot."))
	return nil
}

// logoutCodex drops the Codex credentials. The discovered model list goes
// with them: it is per-account, so leaving it behind would advertise models
// the next account may not have.
func logoutCodex(ws workspace.ConfigAccessor) error {
	if err := cmp.Or(
		ws.RemoveConfigField(config.ScopeGlobal, "providers.codex.api_key"),
		ws.RemoveConfigField(config.ScopeGlobal, "providers.codex.oauth"),
		ws.RemoveConfigField(config.ScopeGlobal, "providers.codex.models"),
	); err != nil {
		return err
	}

	fmt.Println(logoutHeaderStyle.Render("Successfully logged out of OpenAI Codex."))
	return nil
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
