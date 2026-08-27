package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/lipgloss/v2"
	"github.com/pkg/browser"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
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
func loginCodex(ws workspace.ConfigAccessor, force bool, proxyURL string) error {
	loginCtx, stop := getLoginContext()
	defer stop()

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

	// Persisted first, so it lands in ConfiguredProxyURL — the provider-
	// level default an account without a proxy of its own falls back to
	// (see accounts.ResolveProxy) — before RecordAccount below resolves
	// this account's effective route. The account itself is recorded with
	// no proxy of its own (see the LegacyCredential below): were this
	// account's ProxyURL set to the same value directly, a --proxy passed
	// here would be indistinguishable from an override this ONE account
	// happens to want, and a later "give this account no proxy" could no
	// longer tell the two apart. Routing it only through the provider
	// default keeps that one value in one place.
	//
	// Because this write lands before RecordAccount, a failure in either
	// of the two steps below must roll it back — otherwise a login that
	// ultimately failed would still leave the provider's proxy changed
	// with no new account to show for it.
	var previousProxyURL string
	hadProxyURL := false
	if cfg := ws.Config(); cfg != nil {
		if pc, ok := cfg.Providers.Get(codex.ProviderID); ok {
			previousProxyURL = pc.ConfiguredProxyURL
			hadProxyURL = previousProxyURL != ""
		}
	}
	proxyKey := "providers." + codex.ProviderID + ".proxy_url"
	if proxyURL == "" {
		if err := ws.RemoveConfigField(config.ScopeGlobal, proxyKey); err != nil {
			return err
		}
	} else if err := ws.SetConfigField(config.ScopeGlobal, proxyKey, proxyURL); err != nil {
		return err
	}

	accountID := codex.AccountID(token.AccessToken)
	accStore := accounts.NewFileStore(config.GlobalAccountsFile())
	// Counted before RecordAccount so the summary below can tell "a new
	// account appeared" (this login, or the one-time migration of a
	// pre-existing single credential, added an entry) from "the same
	// count as before" (this login only refreshed an account already on
	// file) — RecordAccount itself reports neither, only the resulting
	// Account.
	before, err := accStore.List(codex.ProviderID)
	if err != nil {
		restoreCodexProxyField(ws, hadProxyURL, previousProxyURL)
		return fmt.Errorf("listing existing Codex accounts: %w", err)
	}
	account, err := ws.RecordAccount(config.ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     token,
		AccountID: accountID,
	})
	if err != nil {
		restoreCodexProxyField(ws, hadProxyURL, previousProxyURL)
		return err
	}

	// Which models the account may use is per-plan, so the catalog entry
	// ships without any and the list is fetched here. A failure is not fatal
	// to the sign-in itself: the credentials are already saved, and the list
	// can be refreshed later.
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

	after, err := accStore.List(codex.ProviderID)
	if err != nil {
		// The sign-in itself already succeeded; a failure to re-list
		// afterward only costs the account count in the summary line.
		after = before
	}

	fmt.Println()
	if len(after) > len(before) {
		fmt.Printf("Added the Codex account %q (%d models available).\n", account.Label, len(models))
	} else {
		fmt.Printf("Updated the Codex account %q (%d models available).\n", account.Label, len(models))
	}
	if len(after) > 1 {
		fmt.Printf("You now have %d Codex accounts.\n", len(after))
	}
	return nil
}

// restoreCodexProxyField undoes loginCodex's provider-level proxy_url write
// after a later step (listing accounts, recording the account) fails, so a
// failed login does not leave the proxy setting changed with no new
// account to show for it. Best effort: a failure here is logged rather
// than returned, since the caller already has the real error to report.
func restoreCodexProxyField(ws workspace.ConfigAccessor, hadProxyURL bool, previousProxyURL string) {
	proxyKey := "providers." + codex.ProviderID + ".proxy_url"
	var err error
	if hadProxyURL {
		err = ws.SetConfigField(config.ScopeGlobal, proxyKey, previousProxyURL)
	} else {
		err = ws.RemoveConfigField(config.ScopeGlobal, proxyKey)
	}
	if err != nil {
		slog.Error("Failed to roll back Codex proxy setting after a failed login", "error", err)
	}
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
