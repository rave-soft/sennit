package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/proxyhttp"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/spf13/cobra"
)

// accountsCmd is the CLI surface for the accounts a provider can hold — see
// internal/providers/accounts. Where `sennit login`/`sennit logout` manage
// the single credential a provider used to have, these subcommands manage
// however many of them it has today: which one is active, adding another,
// dropping one, and routing one (or the provider as a whole) through a
// proxy.
// The group is "accounts", not "auth": `auth` is a long-standing alias of
// `sennit login`, so claiming it here would turn `sennit auth codex` — a
// working login command today — into an unknown-subcommand error for
// anyone whose scripts use it.
var accountsCmd = &cobra.Command{
	Use:   "accounts",
	Short: "Manage a provider's stored accounts",
	Long: `Manage the accounts a provider holds credentials for. A provider
can have more than one account — several Codex logins, several API keys for
the same endpoint — and these subcommands list, switch, add, remove and
route them.`,
}

var accountsListCmd = &cobra.Command{
	Use:     "list [provider]",
	Aliases: []string{"ls"},
	Short:   "List accounts, optionally for one provider",
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		if len(args) == 0 {
			return authListAll(ws)
		}
		providerID := normalizeAuthProvider(args[0])
		accts, err := ws.ListAccounts(providerID)
		if err != nil {
			return err
		}
		if len(accts) == 0 {
			fmt.Printf("No accounts for provider %q.\n", providerID)
			return nil
		}
		printAccountList(ws, providerID, accts)
		return nil
	},
}

var accountsUseCmd = &cobra.Command{
	Use:   "use <provider> <account>",
	Short: "Switch a provider to one of its stored accounts",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		return runAuthUse(ws, normalizeAuthProvider(args[0]), args[1])
	},
}

// runAuthUse is accountsUseCmd's RunE body, factored out so a test can drive it
// directly without a *cobra.Command.
func runAuthUse(ws workspace.ConfigAccessor, providerID, accountArg string) error {
	account, err := findAuthAccount(ws, providerID, accountArg)
	if err != nil {
		return err
	}
	if account.Disabled {
		return fmt.Errorf("account %q is disabled for provider %s; enable it before switching to it", account.Label, providerID)
	}
	if err := ws.ActivateAccount(config.ScopeGlobal, providerID, account.ID); err != nil {
		return err
	}
	fmt.Printf("Switched %s to account %q.\n", providerID, account.Label)
	return nil
}

var accountsAddCmd = &cobra.Command{
	Use:   "add <provider>",
	Short: "Add another account to a provider",
	Long: `Add another account to a provider, alongside whatever it already
has. OAuth providers (Codex, Copilot) run through the same sign-in flow as
"sennit login"; API-key providers take --api-key, or prompt for one.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		providerID := normalizeAuthProvider(args[0])
		switch accounts.CapabilitiesOf(providerID).AuthKind {
		case accounts.AuthOAuth:
			return authAddOAuth(ws, providerID)
		default:
			apiKey, _ := cmd.Flags().GetString("api-key")
			return authAddAPIKey(ws, providerID, apiKey)
		}
	},
}

var accountsRemoveCmd = &cobra.Command{
	Use:     "remove <provider> <account>",
	Aliases: []string{"rm"},
	Short:   "Remove an account from a provider",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		return runAuthRemove(ws, normalizeAuthProvider(args[0]), args[1])
	},
}

// runAuthRemove is accountsRemoveCmd's RunE body, factored out so a test can
// drive it directly without a *cobra.Command. Whatever error
// ws.RemoveAccount returns (e.g. its own "last account" refusal, which
// already names `sennit logout`) is propagated as-is, not swallowed or
// replaced with a competing message.
func runAuthRemove(ws workspace.ConfigAccessor, providerID, accountArg string) error {
	account, err := findAuthAccount(ws, providerID, accountArg)
	if err != nil {
		return err
	}
	if err := ws.RemoveAccount(config.ScopeGlobal, providerID, account.ID); err != nil {
		return err
	}
	fmt.Printf("Removed account %q from %s.\n", account.Label, providerID)
	return nil
}

var accountsProxyCmd = &cobra.Command{
	Use:   "proxy <provider> [<account>] <url|none|->",
	Short: "Set or clear a provider's or account's proxy",
	Long: `Set or clear the proxy a provider (or one of its accounts) routes
requests through. "-" clears the setting back to inherit (the provider
falls back to the environment; an account falls back to the provider).
"none" is a distinct value that forces a direct connection instead of
inheriting anything.`,
	Args: cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		providerID := normalizeAuthProvider(args[0])
		var accountArg, rawURL string
		if len(args) == 3 {
			accountArg, rawURL = args[1], args[2]
		} else {
			rawURL = args[1]
		}
		return runAuthProxy(ws, providerID, accountArg, rawURL)
	},
}

// runAuthProxy is accountsProxyCmd's RunE body, factored out so a test can
// drive it directly without a *cobra.Command. accountArg is "" for the
// 2-arg (provider-level) form.
func runAuthProxy(ws workspace.ConfigAccessor, providerID, accountArg, rawURL string) error {
	proxyURL := rawURL
	if proxyURL == "-" {
		proxyURL = ""
	}
	if err := proxyhttp.ValidateProxy(proxyURL); err != nil {
		return err
	}

	if accountArg == "" {
		if err := ws.SetProviderProxy(providerID, proxyURL); err != nil {
			return err
		}
		fmt.Printf("Provider %s now %s.\n", providerID, describeProxy(proxyURL))
		return nil
	}

	account, err := findAuthAccount(ws, providerID, accountArg)
	if err != nil {
		return err
	}
	account.ProxyURL = proxyURL
	if err := ws.UpdateAccount(providerID, account); err != nil {
		return err
	}
	fmt.Printf("Account %q of %s now %s.\n", account.Label, providerID, describeProxy(proxyURL))
	return nil
}

func init() {
	accountsAddCmd.Flags().String("api-key", "", "API key for the account (api-key providers only; prompted for if omitted)")
	accountsCmd.AddCommand(accountsListCmd, accountsUseCmd, accountsAddCmd, accountsRemoveCmd, accountsProxyCmd)
}

// describeProxy renders an effective proxy setting in plain words, for
// confirmation output.
func describeProxy(proxyURL string) string {
	switch {
	case proxyURL == "":
		return "inherits its default route"
	case accounts.IsDirect(proxyURL):
		return "uses a direct connection"
	default:
		return "routes through " + proxyURL
	}
}

// normalizeAuthProvider mirrors the provider aliases loginCmd/logoutCmd
// already accept, so "sennit accounts …" spells provider names the same way.
func normalizeAuthProvider(provider string) string {
	switch provider {
	case "copilot", "github", "github-copilot":
		return "copilot"
	case "codex", "chatgpt", "openai-codex":
		return "codex"
	default:
		return provider
	}
}

// findAuthAccount resolves account (matched against ID, or Label
// case-insensitively) to one of providerID's stored accounts.
func findAuthAccount(ws workspace.ConfigAccessor, providerID, account string) (accounts.Account, error) {
	accts, err := ws.ListAccounts(providerID)
	if err != nil {
		return accounts.Account{}, err
	}
	for _, a := range accts {
		if a.ID == account {
			return a, nil
		}
	}
	for _, a := range accts {
		if strings.EqualFold(a.Label, account) {
			return a, nil
		}
	}
	return accounts.Account{}, fmt.Errorf("no account %q found for provider %s", account, providerID)
}

// authAddOAuth runs the existing OAuth login flow with force semantics
// that skip its "already logged in" short-circuit: RecordAccount's own
// AccountID-matching is what decides add-vs-update for these providers
// (see config.RecordAccount), not the force flag, so forcing here is what
// makes "add" actually attempt a fresh sign-in instead of bailing out
// early because one account already exists.
func authAddOAuth(ws workspace.ConfigAccessor, providerID string) error {
	switch providerID {
	case "codex":
		return loginCodex(ws, true, "")
	case "copilot":
		return loginCopilot(ws, true)
	default:
		return fmt.Errorf("provider %s has no OAuth sign-in flow", providerID)
	}
}

// authAddAPIKey records a new api-key account for providerID. apiKey, if
// empty, is prompted for interactively. ForceNewAccount is set so a
// provider with no AccountID of its own (every api-key provider) gets a
// genuinely new account instead of RecordAccount folding this into the
// provider's existing active one — see config.RecordAccount's doc comment,
// step 4.
func authAddAPIKey(ws workspace.ConfigAccessor, providerID, apiKey string) error {
	if apiKey == "" {
		fmt.Printf("API key for %s: ", providerID)
		key, err := readSecretLine(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading API key: %w", err)
		}
		apiKey = key
	}
	if apiKey == "" {
		return fmt.Errorf("API key for %s must not be empty", providerID)
	}
	account, err := ws.RecordAccount(config.ScopeGlobal, providerID, accounts.LegacyCredential{
		APIKey:          apiKey,
		ForceNewAccount: true,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Added the %s account %q.\n", providerID, account.Label)
	return nil
}

// readSecretLine reads one line of secret input from in without echoing it
// to the terminal. Unlike fmt.Scanln into a single string, it reads the
// whole line, so a key containing spaces survives intact; only the trailing
// newline and surrounding whitespace are trimmed. When in isn't a terminal
// (piped input, CI), there's no echo to suppress, so it falls back to a
// plain line read instead of failing.
func readSecretLine(in *os.File) (string, error) {
	if term.IsTerminal(in.Fd()) {
		b, err := term.ReadPassword(in.Fd())
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// authListAll lists every provider with at least one stored account.
func authListAll(ws workspace.ConfigAccessor) error {
	cfg := ws.Config()
	if cfg == nil || cfg.Providers == nil {
		fmt.Println("No accounts stored for any provider.")
		return nil
	}

	found := false
	for providerID := range cfg.Providers.Seq2() {
		accts, err := ws.ListAccounts(providerID)
		if err != nil {
			return err
		}
		if len(accts) == 0 {
			continue
		}
		found = true
		printAccountList(ws, providerID, accts)
	}
	if !found {
		fmt.Println("No accounts stored for any provider.")
	}
	return nil
}

var (
	authHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	authItemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	authMutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

// printAccountList renders providerID's accounts: label, active/disabled
// markers, and — only for a provider that reports usage
// (accounts.CapabilitiesOf) — its stored allowance figures.
func printAccountList(ws workspace.ConfigAccessor, providerID string, accts []accounts.Account) {
	activeID := ""
	if cfg := ws.Config(); cfg != nil {
		if pc, ok := cfg.Providers.Get(providerID); ok {
			activeID = pc.Account
		}
	}
	showUsage := accounts.CapabilitiesOf(providerID).Usage

	fmt.Println(authHeaderStyle.Render(providerID + ":"))
	for _, a := range accts {
		label := a.Label
		if label == "" {
			label = a.ID
		}
		markers := []string{}
		if a.ID == activeID {
			markers = append(markers, "active")
		}
		if a.Disabled {
			markers = append(markers, "disabled")
		}
		line := fmt.Sprintf("  %s", label)
		if len(markers) > 0 {
			line += " (" + strings.Join(markers, ", ") + ")"
		}
		fmt.Println(authItemStyle.Render(line))
		if showUsage && a.Usage.Known() {
			fmt.Println(authMutedStyle.Render("    " + formatAccountUsage(a.Usage)))
		}
	}
}

// formatAccountUsage renders a single account's stored allowance snapshot
// in plain words.
func formatAccountUsage(u accounts.Usage) string {
	parts := []string{}
	if u.Plan != "" {
		parts = append(parts, u.Plan)
	}
	if u.Primary.Known() {
		parts = append(parts, fmt.Sprintf("primary %d%% used", u.Primary.UsedPercent))
	} else {
		parts = append(parts, "primary unknown")
	}
	if u.Secondary.Known() {
		parts = append(parts, fmt.Sprintf("secondary %d%% used", u.Secondary.UsedPercent))
	}
	return strings.Join(parts, " · ")
}
