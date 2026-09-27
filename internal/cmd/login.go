package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"charm.land/lipgloss/v2"
	"github.com/pkg/browser"
	"github.com/rave-soft/sennit/internal/clipboard"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/spf13/cobra"
)

type oauthPlatform struct {
	ID          string
	DisplayName string
	Aliases     []string
	Login       func(workspace.Workspace, bool, string) error
	Logout      func(workspace.Workspace) error
}

var oauthPlatforms = []oauthPlatform{
	{
		ID: "copilot", DisplayName: "GitHub Copilot", Aliases: []string{"github", "github-copilot"},
		Login: func(ws workspace.Workspace, force bool, _ string) error {
			return loginCopilot(ws, force, false, desktopLoginIO())
		},
		Logout: func(ws workspace.Workspace) error { return logoutCopilot(ws) },
	},
	{
		ID: "codex", DisplayName: "OpenAI Codex", Aliases: []string{"chatgpt", "openai-codex"},
		Login: func(ws workspace.Workspace, force bool, proxyURL string) error {
			return loginCodex(ws, force, false, proxyURL, desktopLoginIO())
		},
		Logout: func(ws workspace.Workspace) error { return logoutCodex(ws) },
	},
}

func resolveOAuthPlatform(value string) (oauthPlatform, bool) {
	for _, platform := range oauthPlatforms {
		if value == platform.ID {
			return platform, true
		}
		for _, alias := range platform.Aliases {
			if value == alias {
				return platform, true
			}
		}
	}
	return oauthPlatform{}, false
}

func oauthPlatformCompletions() []cobra.Completion {
	values := make([]cobra.Completion, 0, len(oauthPlatforms)*2)
	for _, platform := range oauthPlatforms {
		values = append(values, platform.ID)
		values = append(values, platform.Aliases...)
	}
	return values
}

var loginCmd = &cobra.Command{
	Aliases: []string{"auth"},
	Use:     "login [platform]",
	Short:   "Login Sennit to a platform",
	Long: `Login Sennit to a specified platform.
The platform should be provided as an argument.
Available platforms are: copilot, codex.`,
	Example: `
# Authenticate with GitHub Copilot
sennit login copilot

# Authenticate with OpenAI Codex using a ChatGPT subscription
sennit login codex

# Authenticate with OpenAI Codex through a proxy
sennit login codex --proxy socks5://127.0.0.1:1080

# Force re-authentication even if already logged in
sennit login -f copilot
  `,
	ValidArgs: oauthPlatformCompletions(),
	Args:      cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupAccountWorkspace(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		provider := "copilot"
		if len(args) > 0 {
			provider = args[0]
		}
		force, _ := cmd.Flags().GetBool("force")
		platform, ok := resolveOAuthPlatform(provider)
		if !ok {
			return fmt.Errorf("unknown platform: %s", provider)
		}
		proxyURL, _ := cmd.Flags().GetString("proxy")
		return platform.Login(ws, force, proxyURL)
	},
}

func init() {
	loginCmd.Flags().BoolP("force", "f", false, "Force re-authentication even if already logged in")
	loginCmd.Flags().String("proxy", "", `Proxy for reaching the platform, e.g. http://host:port or socks5://host:port ("none" forces a direct connection). Codex only; saved with the provider so model requests use it too`)
}

// loginCopilot signs Sennit in to GitHub Copilot. forceNewAccount is
// threaded through to StartOAuth, which passes it on to RecordAccount once
// the flow completes: Copilot's OAuth token carries no account identifier
// of its own (unlike Codex's JWT), so RecordAccount has no way to tell
// "this is the same sign-in, refresh it" from "this is a deliberate second
// account" without being told explicitly — see
// accounts.LegacyCredential.ForceNewAccount's doc comment. `sennit login
// copilot` passes false (a routine re-login updates the existing account);
// `sennit accounts add copilot`, via authAddOAuth, passes true.
//
// The sign-in itself lives behind the workspace now (see
// workspace.OAuthController, implemented in internal/workspace/appws):
// running the device flow, recording the account and persisting it are all
// one implementation shared with the TUI's sign-in dialog, mirroring
// login_codex.go. What stays here is the console half — narrating the
// device code and URL, and the interactive "press enter to open the
// browser" step a UI has no use for - plus one CLI-only shortcut,
// ImportCopilot, that reuses a GitHub Copilot CLI login already on disk
// (see the comment where it is called, below).
type loginAccountWorkspace interface {
	workspace.ConfigReader
	workspace.ConfigFieldEditor
	workspace.AccountLister
	// OAuthController is what authAddOAuth's loginCodex/loginCopilot need:
	// the sign-in flow itself lives behind the workspace now.
	workspace.OAuthController
}

func loginCopilot(ws loginAccountWorkspace, force, forceNewAccount bool, io loginIO) error {
	loginCtx, stop := getLoginContext()
	defer stop()

	// A proxy already configured for this provider (e.g. set by hand in
	// config, or left over from a previous sign-in) should route this
	// sign-in too: the model calls it will make afterwards use it, and a
	// sign-in that ignored it would fail while the provider looked
	// correctly configured.
	//
	// This reads OAuthConfiguredProxy, a Workspace method, rather than
	// ws.Config()'s FrontendProvider.ProxyURL: that field is redacted for
	// display (password stripped, see redactProxyURL), which would
	// silently break a proxy credential this value is about to route the
	// sign-in through. See workspace.OAuthController.OAuthConfiguredProxy.
	proxyURL := ws.OAuthConfiguredProxy("copilot")
	if cfg := ws.Config(); cfg != nil {
		if pc, ok := cfg.Provider("copilot"); ok && !force && pc.Auth.HasOAuth {
			fmt.Println("You are already logged in to GitHub Copilot.")
			fmt.Println("Use --force to re-authenticate.")
			return nil
		}
	}

	// ImportCopilot mirrors the on-disk refresh-token shortcut this
	// command used to run itself (copilot.RefreshTokenFromDisk +
	// copilot.RefreshToken): a GitHub Copilot CLI login already on this
	// machine is imported without a device flow. It now lives behind the
	// workspace (internal/config/credentials.Manager.ImportCopilot),
	// which persists straight to providers.copilot.oauth and reports
	// (false, nil) - not an error - the moment Copilot has ANY credential
	// configured, with no notion of "which account".
	//
	// That makes it the wrong tool for a deliberate "add account"
	// (forceNewAccount): reusing it there would either silently do
	// nothing (an account is already configured, which is the ordinary
	// case for "add another") or, on a first-ever login run with
	// --force-new-account for some reason, record the disk login outside
	// RecordAccount's multi-account bookkeeping entirely. `sennit
	// accounts add copilot` therefore always goes through the device
	// flow below, which threads forceNewAccount into RecordAccount
	// properly; only a routine `sennit login copilot` tries the disk
	// shortcut first.
	if !forceNewAccount {
		imported, err := ws.ImportCopilot(loginCtx)
		if err != nil {
			return err
		}
		if imported {
			fmt.Println("Found existing GitHub Copilot token on disk. Using it to authenticate...")
			fmt.Println()
			fmt.Println("You're now authenticated with GitHub Copilot!")
			return nil
		}
	}

	result, flow, err := ws.StartOAuth(loginCtx, "copilot", proxyURL, forceNewAccount)
	if err != nil {
		return err
	}
	if flow != nil {
		defer flow.Cancel()
	}

	if result.Completed == nil {
		fmt.Println("Requesting device code from GitHub...")

		io.copyText(result.UserCode)
		fmt.Println()
		fmt.Println("The following code should be on clipboard already:")
		fmt.Println()
		_, _ = lipgloss.Println(lipgloss.NewStyle().Bold(true).Render(result.UserCode)) // terminal output
		fmt.Println()
		fmt.Println("Press enter to open this URL and authenticate with GitHub Copilot:")
		fmt.Println()
		_, _ = lipgloss.Println(lipgloss.NewStyle().Hyperlink(result.VerificationURL, "id=copilot").Render(result.VerificationURL)) // terminal output
		fmt.Println()
		io.waitEnter()
		if err := io.openURL(result.VerificationURL); err != nil {
			fmt.Println("Could not open the URL. You'll need to manually open the URL in your browser.")
		}

		fmt.Println("Waiting for authorization...")

		_, err = flow.Wait(loginCtx)
		if errors.Is(err, copilot.ErrNotAvailable) {
			fmt.Println()
			fmt.Println("GitHub Copilot is unavailable for this account. To signup, go to the following page:")
			fmt.Println()
			_, _ = lipgloss.Println(lipgloss.NewStyle().Hyperlink(copilot.SignupURL, "id=copilot-signup").Render(copilot.SignupURL)) // terminal output
			fmt.Println()
			fmt.Println("You may be able to request free access if eligible. For more information, see:")
			fmt.Println()
			_, _ = lipgloss.Println(lipgloss.NewStyle().Hyperlink(copilot.FreeURL, "id=copilot-free").Render(copilot.FreeURL)) // terminal output
		}
		if err != nil {
			return err
		}
	}

	fmt.Println()
	fmt.Println("You're now authenticated with GitHub Copilot!")
	return nil
}

// getLoginContext returns a context cancelled by an interrupt, so a
// sign-in flow that is waiting on a browser round-trip can unwind through
// its own deferred cleanup (the callback listener, the temporary
// credentials) instead of being cut off mid-way.
//
// os.Kill was listed here and is not catchable — SIGKILL never reaches a
// handler — so a `kill` of the process left the flow no chance to clean
// up at all; SIGTERM is the one that can be handled. The os.Exit that
// used to fire from a goroutine is gone with it: it skipped every defer
// in the flow, including the listener close that frees the fixed callback
// port. NotifyContext stops trapping after the first signal, so a second
// interrupt still ends the process immediately.
//
// The returned stop func must be deferred by the caller: NotifyContext
// leaves its os/signal registration in place until stop is called, so a
// caller that discarded it (as this used to) leaked a signal handler on
// every login attempt in a process that runs more than one, e.g. tests.
func getLoginContext() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// loginIO is everything an interactive sign-in does outside the process:
// put a code on the clipboard, wait for Enter, open a browser. Production
// passes desktopLoginIO; tests pass a recorder. A test reaching the real
// ones opens tabs in the person's browser and overwrites their clipboard
// on every run, which is what happened before this indirection existed.
// TestLoginDesktopEffectsOnlyInDesktopLoginIO keeps new call sites out.
type loginIO struct {
	copyText  func(string)
	waitEnter func()
	openURL   func(string) error
}

func desktopLoginIO() loginIO {
	return loginIO{
		copyText:  clipboard.WriteText,
		waitEnter: waitEnter,
		openURL:   browser.OpenURL,
	}
}

func waitEnter() {
	_, _ = fmt.Scanln()
}
