package modelsrefresh

import (
	"context"
	"errors"
	"fmt"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
)

// ErrCodexSignedOut is returned when a Codex refresh is requested with no
// Codex login. Refresh re-reads a list that a sign-in created; the fix is
// a sign-in.
var ErrCodexSignedOut = errors.New("not signed in to Codex; run `sennit login codex`")

// ContextWindowChange records a model whose context window differs
// between the stored and the fetched Codex list.
type ContextWindowChange struct {
	ID       string
	Old, New int64
}

// CodexConfigured reports whether there is a Codex login to refresh models
// for. It keys off the OAuth token: the provider entry can exist (a
// proxy_url set by hand, say) with no login behind it.
func CodexConfigured(cfg *config.ConfigStore) bool {
	current := cfg.Config()
	if current == nil {
		return false
	}
	pc, ok := current.RuntimeProvider(codex.ProviderID)
	return ok && pc.OAuthToken != nil
}

// RefreshCodex re-reads the Codex model list and overwrites
// providers.codex.models in the global config.
//
// Codex is not a custom provider: it has no /models endpoint of the
// OpenAI-compatible kind the rest of refresh discovers against, and its
// list is written into the config at sign-in rather than into the
// model-discovery cache. Without this path the only way to pick up a
// changed list (a new model on the account, or a field Sennit has started
// reading, as max_context_window was) is to sign in again.
//
// It returns ErrCodexSignedOut without a login; every other failure lands
// in Result.Err.
func RefreshCodex(ctx context.Context, cfg *config.ConfigStore) (Result, error) {
	result := Result{ID: codex.ProviderID}
	current := cfg.Config()
	if current == nil {
		return result, fmt.Errorf("configuration is unavailable")
	}
	// Models comes off the disk-shaped entry (what refresh is about to
	// diff against and overwrite); the credential and effective proxy
	// come off the runtime provider, the only view that carries them.
	pc, ok := current.Providers.Get(codex.ProviderID)
	if !ok {
		return result, ErrCodexSignedOut
	}
	cred, ok := current.RuntimeProvider(codex.ProviderID)
	if !ok || cred.OAuthToken == nil {
		return result, ErrCodexSignedOut
	}

	token, err := codexAccessToken(ctx, cfg, cred)
	if err != nil {
		result.Err = err
		return result, nil
	}

	models, err := codex.FetchModels(ctx, cred.ProxyURL, token.AccessToken, codex.AccountID(token.AccessToken))
	if err != nil {
		result.Err = err
		return result, nil
	}

	result.Models = len(models)
	result.Added, result.Removed = DiffModelIDs(pc.Models, models)

	// DiffModelIDs only tracks IDs; a context-window change lands as
	// neither an add nor a remove, so it needs its own pass over the full
	// values.
	existing := make(map[string]catwalk.Model, len(pc.Models))
	for _, m := range pc.Models {
		existing[m.ID] = m
	}
	for _, m := range models {
		if old, known := existing[m.ID]; known && old.ContextWindow != m.ContextWindow {
			result.ContextWindowChanges = append(result.ContextWindowChanges,
				ContextWindowChange{ID: m.ID, Old: old.ContextWindow, New: m.ContextWindow})
		}
	}

	if err := cfg.SetConfigField(config.ScopeGlobal, "providers."+codex.ProviderID+".models", models); err != nil {
		result.Err = err
	}
	return result, nil
}

// codexAccessToken returns an access token usable for the model-list call.
//
// The stored one is used while it is valid. Past that, the Codex CLI's own
// token is preferred over an exchange: Codex refresh tokens are single-use,
// so spending ours to list models would log out whichever tool holds the
// older one. An exchange is the last resort, and its result is persisted;
// a rotation that is not written down strands the next refresh.
func codexAccessToken(ctx context.Context, cfg *config.ConfigStore, cred providerstate.Provider) (*oauth.Token, error) {
	if !cred.OAuthToken.IsExpired() {
		return cred.OAuthToken, nil
	}
	if token, ok := codex.TokenFromDiskFor(codex.AccountID(cred.APIKey)); ok && !token.IsExpired() {
		return token, nil
	}
	if cred.OAuthToken.RefreshToken == "" {
		return nil, fmt.Errorf("the Codex login has expired and cannot be refreshed; run `sennit login codex -f`")
	}
	token, err := codex.RefreshToken(ctx, cred.ProxyURL, cred.OAuthToken.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("could not refresh the Codex login: %w", err)
	}
	if err := cfg.SetProviderAPIKey(config.ScopeGlobal, codex.ProviderID, token); err != nil {
		return nil, err
	}
	return token, nil
}
