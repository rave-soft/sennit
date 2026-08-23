package cmd

import (
	"context"
	"fmt"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/spf13/cobra"
)

// codexConfigured reports whether there is a Codex login to refresh models
// for. Refresh is about re-reading a list that already exists; signing in is
// `sennit login codex`.
func codexConfigured(cfg *config.ConfigStore) bool {
	current := cfg.Config()
	if current == nil || current.Providers == nil {
		return false
	}
	pc, ok := current.Providers.Get(codex.ProviderID)
	return ok && pc.OAuthToken != nil
}

// refreshCodexModels re-reads the Codex model list and overwrites
// providers.codex.models in the global config.
//
// Codex is not a custom provider — it has no /models endpoint of the
// OpenAI-compatible kind the rest of refresh discovers against, and its list
// is written into the config at sign-in rather than into the model-discovery
// cache. Without this path the only way to pick up a changed list (a new
// model on the account, or a field Sennit has started reading, as
// max_context_window was) is to sign in again.
func refreshCodexModels(ctx context.Context, cmd *cobra.Command, cfg *config.ConfigStore) error {
	pc, ok := cfg.Config().Providers.Get(codex.ProviderID)
	if !ok || pc.OAuthToken == nil {
		return fmt.Errorf("not signed in to Codex; run `sennit login codex`")
	}

	token, err := codexRefreshToken(ctx, cfg, pc)
	if err != nil {
		return err
	}

	models, err := codex.FetchModels(ctx, pc.ProxyURL, token.AccessToken, codex.AccountID(token.AccessToken))
	if err != nil {
		return err
	}

	added, removed := diffModelIDs(pc.Models, models)

	// diffModelIDs only tracks IDs; a context-window change lands as neither
	// an add nor a remove, so it needs its own pass over the full values.
	existing := make(map[string]catwalk.Model, len(pc.Models))
	for _, m := range pc.Models {
		existing[m.ID] = m
	}
	var changed int
	for _, m := range models {
		if old, known := existing[m.ID]; known && old.ContextWindow != m.ContextWindow {
			changed++
			cmd.Printf("  %s: context window %d → %d\n", m.ID, old.ContextWindow, m.ContextWindow)
		}
	}

	if err := cfg.SetConfigField(config.ScopeGlobal, "providers."+codex.ProviderID+".models", models); err != nil {
		return err
	}
	cmd.Printf("%s: %d models (+%d new, -%d removed, %d updated)\n",
		codex.ProviderID, len(models), added, removed, changed)
	return nil
}

// codexRefreshToken returns an access token usable for the model-list call.
//
// The stored one is used while it is valid. Past that, the Codex CLI's own
// token is preferred over an exchange: Codex refresh tokens are single-use,
// so spending ours to list models would log out whichever tool holds the
// older one. An exchange is the last resort, and its result is persisted —
// a rotation that is not written down strands the next refresh.
func codexRefreshToken(ctx context.Context, cfg *config.ConfigStore, pc config.ProviderConfig) (*oauth.Token, error) {
	if !pc.OAuthToken.IsExpired() {
		return pc.OAuthToken, nil
	}
	if token, ok := codex.TokenFromDiskFor(codex.AccountID(pc.APIKey)); ok && !token.IsExpired() {
		return token, nil
	}
	if pc.OAuthToken.RefreshToken == "" {
		return nil, fmt.Errorf("the Codex login has expired and cannot be refreshed; run `sennit login codex -f`")
	}
	token, err := codex.RefreshToken(ctx, pc.ProxyURL, pc.OAuthToken.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("could not refresh the Codex login: %w", err)
	}
	if err := cfg.SetProviderAPIKey(config.ScopeGlobal, codex.ProviderID, token); err != nil {
		return nil, err
	}
	return token, nil
}
