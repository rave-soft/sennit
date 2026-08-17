package cmd

import (
	"context"
	"fmt"

	"charm.land/lipgloss/v2"
	"github.com/pkg/browser"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/workspace"
)

// loginCodex signs Sennit in to OpenAI Codex with a ChatGPT subscription.
//
// An existing Codex CLI login is used when there is one: the refresh token it
// left on disk is exchanged for Sennit's own pair, which spares the user a
// second browser round trip. Otherwise the browser flow runs.
//
// proxyURL is optional, and routes both halves of the sign-in as well as the
// model requests that follow: for a user who can only reach OpenAI through a
// proxy, a token exchange that ignored it would fail on its own.
func loginCodex(ws workspace.Workspace, force bool, proxyURL string) error {
	loginCtx := getLoginContext()

	if err := codex.ValidateProxy(proxyURL); err != nil {
		return err
	}
	// An unspecified proxy keeps whatever the provider is already
	// configured with, so re-running login does not silently drop it, and
	// otherwise borrows the Codex CLI's — someone behind a proxy has
	// already told it about theirs.
	if proxyURL == "" {
		if cfg := ws.Config(); cfg != nil {
			if pc, ok := cfg.Providers.Get(codex.ProviderID); ok {
				proxyURL = pc.ProxyURL
			}
		}
	}
	if proxyURL == "" {
		if fromCLI := codex.ProxyFromDisk(); fromCLI != "" {
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

	token, err := codexToken(loginCtx, proxyURL)
	if err != nil {
		return err
	}

	if err := ws.SetProviderAPIKey(config.ScopeGlobal, codex.ProviderID, token); err != nil {
		return err
	}
	// Persisted so the model requests take the same route the sign-in did.
	proxyKey := "providers." + codex.ProviderID + ".proxy_url"
	if proxyURL == "" {
		if err := ws.RemoveConfigField(config.ScopeGlobal, proxyKey); err != nil {
			return err
		}
	} else if err := ws.SetConfigField(config.ScopeGlobal, proxyKey, proxyURL); err != nil {
		return err
	}

	// Which models the account may use is per-plan, so the catalog entry
	// ships without any and the list is fetched here. A failure is not fatal
	// to the sign-in itself: the credentials are already saved, and the list
	// can be refreshed later.
	accountID := codex.AccountID(token.AccessToken)
	models, err := codex.FetchModels(loginCtx, proxyURL, token.AccessToken, accountID)
	if err != nil {
		fmt.Println()
		fmt.Println("Signed in, but the model list could not be fetched:", err)
		fmt.Println("Run `sennit login codex -f` to try again.")
		return nil
	}
	if err := ws.SetConfigField(config.ScopeGlobal, "providers."+codex.ProviderID+".models", models); err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("You're now authenticated with OpenAI Codex (%d models available)!\n", len(models))
	return nil
}

// codexToken obtains a token, preferring an existing Codex CLI login on disk
// over the browser flow.
func codexToken(ctx context.Context, proxyURL string) (*oauth.Token, error) {
	if disk, ok := codex.TokensFromDisk(); ok {
		fmt.Println("Found an existing Codex CLI login on disk. Using it to authenticate...")

		// Prefer the access token it already holds. Refreshing would
		// consume the CLI's single-use refresh token and log it out — a
		// rude thing to do to another tool on the way past.
		if token, ok := disk.Token(); ok {
			return token, nil
		}

		token, err := codex.RefreshToken(ctx, proxyURL, disk.RefreshToken)
		if err == nil {
			fmt.Println("The Codex CLI's token was close to expiring, so it was refreshed;")
			fmt.Println("the CLI may ask you to sign in again the next time you use it.")
			return token, nil
		}
		// A stale or revoked token on disk is not a reason to fail: the
		// browser flow below is exactly the fallback for it.
		fmt.Println("Could not reuse the Codex CLI login:", err)
	}

	flow, err := codex.StartFlow(proxyURL)
	if err != nil {
		return nil, err
	}
	defer flow.Close() // best-effort listener shutdown

	fmt.Println()
	fmt.Println("Press enter to open this URL and sign in with your ChatGPT account:")
	fmt.Println()
	_, _ = lipgloss.Println(lipgloss.NewStyle().Hyperlink(flow.URL(), "id=codex").Render(flow.URL())) // terminal output
	fmt.Println()
	waitEnter()
	if err := browser.OpenURL(flow.URL()); err != nil {
		fmt.Println("Could not open the URL. You'll need to manually open the URL in your browser.")
	}

	fmt.Println("Waiting for authorization...")
	return flow.Wait(ctx)
}
