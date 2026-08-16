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
func loginCodex(ws workspace.Workspace, force bool) error {
	loginCtx := getLoginContext()

	if !force {
		if cfg := ws.Config(); cfg != nil {
			if pc, ok := cfg.Providers.Get(codex.ProviderID); ok && pc.OAuthToken != nil {
				fmt.Println("You are already logged in to OpenAI Codex.")
				fmt.Println("Use --force to re-authenticate.")
				return nil
			}
		}
	}

	token, err := codexToken(loginCtx)
	if err != nil {
		return err
	}

	if err := ws.SetProviderAPIKey(config.ScopeGlobal, codex.ProviderID, token); err != nil {
		return err
	}

	// Which models the account may use is per-plan, so the catalog entry
	// ships without any and the list is fetched here. A failure is not fatal
	// to the sign-in itself: the credentials are already saved, and the list
	// can be refreshed later.
	accountID := codex.AccountID(token.AccessToken)
	models, err := codex.FetchModels(loginCtx, token.AccessToken, accountID)
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
func codexToken(ctx context.Context) (*oauth.Token, error) {
	if disk, ok := codex.TokensFromDisk(); ok {
		fmt.Println("Found an existing Codex CLI login on disk. Using it to authenticate...")
		token, err := codex.RefreshToken(ctx, disk.RefreshToken)
		if err == nil {
			return token, nil
		}
		// A stale or revoked token on disk is not a reason to fail: the
		// browser flow below is exactly the fallback for it.
		fmt.Println("Could not reuse the Codex CLI login:", err)
	}

	flow, err := codex.StartFlow()
	if err != nil {
		return nil, err
	}
	defer flow.Close() //nolint:errcheck // best-effort listener shutdown

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
