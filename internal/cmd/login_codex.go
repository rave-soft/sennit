package cmd

import (
	"fmt"

	"charm.land/lipgloss/v2"
	"github.com/pkg/browser"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/workspace"
)

// loginCodex signs Sennit in to OpenAI Codex with a ChatGPT subscription.
//
// The sign-in itself lives behind the workspace (see
// workspace.OAuthController, implemented in internal/workspace/appws):
// reusing an existing Codex CLI login, running the browser flow, recording
// the account and persisting the proxy and model list are all one
// implementation shared with the TUI's sign-in dialog. What stays here is
// the console half — narrating what the backend did, and the interactive
// "press enter to open the browser" step a UI has no use for.
//
// proxyURL is optional, and routes both halves of the sign-in as well as the
// model requests that follow: for a user who can only reach OpenAI through a
// proxy, a token exchange that ignored it would fail on its own.
type codexLoginWorkspace interface {
	workspace.ConfigReader
	workspace.AccountLister
	workspace.OAuthController
}

func loginCodex(ws codexLoginWorkspace, force bool, proxyURL string) error {
	loginCtx, stop := getLoginContext()
	defer stop()

	if err := ws.OAuthValidateProxy(codex.ProviderID, proxyURL); err != nil {
		return err
	}
	// An unspecified proxy keeps whatever the provider is already
	// configured with, so re-running login does not silently drop it, and
	// otherwise borrows the Codex CLI's — someone behind a proxy has
	// already told it about theirs. The two halves are resolved
	// separately, even though OAuthConfiguredProxy already falls back from
	// one to the other, so the "borrowed it from the CLI" case can still
	// say so: with nothing configured for the provider, whatever
	// OAuthConfiguredProxy answers can only have come from the CLI's own
	// config file.
	if proxyURL == "" {
		proxyURL = configuredCodexProxy(ws)
	}
	if proxyURL == "" {
		if fromCLI := ws.OAuthConfiguredProxy(codex.ProviderID); fromCLI != "" {
			proxyURL = fromCLI
			fmt.Printf("Using the proxy configured for the Codex CLI: %s\n", proxyURL)
		}
	}

	if !force {
		if cfg := ws.Config(); cfg != nil {
			if pc, ok := cfg.Providers.Get(codex.ProviderID); ok && pc.OAuthToken != nil {
				fmt.Println("You are already logged in to OpenAI Codex.")
				fmt.Println("Use --force to re-authenticate.")
				return nil
			}
		}
	}

	result, flow, err := ws.StartOAuth(loginCtx, codex.ProviderID, proxyURL)
	if err != nil {
		return err
	}
	if flow != nil {
		defer flow.Cancel() // best-effort listener shutdown
	}

	token := result.Token
	if token == nil {
		if result.ExistingLoginFailure != "" {
			// A login was found on disk but could not be reused; the
			// browser flow below is exactly the fallback for that.
			fmt.Println("Found an existing Codex CLI login on disk. Using it to authenticate...")
			fmt.Println("Could not reuse the Codex CLI login:", result.ExistingLoginFailure)
		}

		fmt.Println()
		fmt.Println("Press enter to open this URL and sign in with your ChatGPT account:")
		fmt.Println()
		_, _ = lipgloss.Println(lipgloss.NewStyle().Hyperlink(result.AuthorizationURL, "id=codex").Render(result.AuthorizationURL)) // terminal output
		fmt.Println()
		waitEnter()
		if err := browser.OpenURL(result.AuthorizationURL); err != nil {
			fmt.Println("Could not open the URL. You'll need to manually open the URL in your browser.")
		}

		fmt.Println("Waiting for authorization...")
		token, err = flow.Wait(loginCtx)
		if err != nil {
			return err
		}
	} else {
		fmt.Println("Found an existing Codex CLI login on disk. Using it to authenticate...")
		if result.RefreshedExistingLogin {
			// Refreshing spends the CLI's single-use refresh token, so say
			// what it cost the other tool.
			fmt.Println("The Codex CLI's token was close to expiring, so it was refreshed;")
			fmt.Println("the CLI may ask you to sign in again the next time you use it.")
		}
	}

	// Counted before the account is recorded so the summary below can tell
	// "a new account appeared" (this login, or the one-time migration of a
	// pre-existing single credential, added an entry) from "the same count
	// as before" (this login only refreshed an account already on file) —
	// CompleteOAuth itself reports neither, only the resulting Account.
	before, err := ws.ListAccounts(codex.ProviderID)
	if err != nil {
		return fmt.Errorf("listing existing Codex accounts: %w", err)
	}

	completion, err := ws.CompleteOAuth(loginCtx, codex.ProviderID, proxyURL, token, false)
	if err != nil {
		return err
	}

	// A proxy that could not be persisted as the provider's default fails
	// the command, matching what a failed proxy write did before this
	// refactor (it aborted the login outright, before the account was
	// even recorded) — the credential just happens to already be saved
	// this time. completion.ProxyError already reads as a complete
	// sentence (see AppWorkspace.CompleteOAuth), so it is returned as-is
	// rather than wrapped again.
	if completion.ProxyError != nil {
		return completion.ProxyError
	}

	// Which models the account may use is per-plan, so the catalog entry
	// ships without any and the list is fetched during CompleteOAuth. A
	// failure is not fatal to the sign-in itself: the credentials are
	// already saved, and the list can be refreshed later.
	// completion.ModelsError, like ProxyError above, already reads as a
	// complete sentence and is printed as-is.
	if completion.ModelsError != nil {
		fmt.Println()
		fmt.Println(completion.ModelsError)
		fmt.Println("Run `sennit login codex -f` to try again.")
		return nil
	}

	after, err := ws.ListAccounts(codex.ProviderID)
	if err != nil {
		// The sign-in itself already succeeded; a failure to re-list
		// afterward only costs the account count in the summary line.
		after = before
	}

	fmt.Println()
	if len(after) > len(before) {
		fmt.Printf("Added the Codex account %q (%d models available).\n", completion.Account.Label, completion.ModelsFetched)
	} else {
		fmt.Printf("Updated the Codex account %q (%d models available).\n", completion.Account.Label, completion.ModelsFetched)
	}
	if len(after) > 1 {
		fmt.Printf("You now have %d Codex accounts.\n", len(after))
	}
	return nil
}

// configuredCodexProxy returns the provider-level proxy the Codex provider
// is already configured with, or "" if none.
//
// It reads ConfiguredProxyURL, not ProxyURL: ProxyURL is the *effective*
// proxy — whatever the currently active account resolved to, which may be
// that account's own override, or "none" forcing a direct connection (see
// accounts.ResolveProxy) — while ConfiguredProxyURL is the provider-level
// default as written in config. loginCodex falls back to this value when
// --proxy is not passed, and CompleteOAuth then persists it back to
// providers.codex.proxy_url; using the effective value there would promote
// one account's route to every account's default on the next login, and
// would rewrite a "$VAR" template to its resolved literal even though
// nothing asked for a proxy change at all.
func configuredCodexProxy(ws workspace.ConfigReader) string {
	cfg := ws.Config()
	if cfg == nil {
		return ""
	}
	pc, ok := cfg.Providers.Get(codex.ProviderID)
	if !ok {
		return ""
	}
	return pc.ProxyURL
}
