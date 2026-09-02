package appws

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/tidwall/gjson"
)

// copilotProviderID is copilot's provider id. Unlike Codex, the copilot
// package exports no such constant of its own.
const copilotProviderID = "copilot"

// setCodexProxyConfigField/removeCodexProxyConfigField are package vars
// wrapping (*AppWorkspace).SetConfigField/RemoveConfigField, used only for
// completeCodexOAuth's proxy write. They exist so a test can force that one
// write to fail without a real disk failure, which would also break the
// RecordAccount call immediately before it — both go through the same
// config file. Mirrors internal/cmd/login_codex_test.go's
// codexTokenForLogin/fetchCodexModels swap.
var (
	setCodexProxyConfigField    = (*AppWorkspace).SetConfigField
	removeCodexProxyConfigField = (*AppWorkspace).RemoveConfigField
)

// previousCodexProxyURL reads providers.codex.proxy_url straight from the
// global config file, bypassing the store's in-memory Config() — see
// completeCodexOAuth's comment on why that snapshot cannot be trusted for
// this. "" (including "not found" and "cannot even locate the file") is
// treated the same as "nothing configured": both mean the guard should
// compare against an empty previous value, which is the safe direction to
// be wrong in — at worst it attempts a write that turns out to be
// unnecessary, never a skip that should have written.
func previousCodexProxyURL(w *AppWorkspace) string {
	path, err := w.store.ConfigPath(config.ScopeGlobal)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return gjson.GetBytes(data, "providers.codex.proxy_url").String()
}

// -- OAuthController --

// StartOAuth implements workspace.OAuthController.
func (w *AppWorkspace) StartOAuth(ctx context.Context, providerID, proxyURL string) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	switch providerID {
	case codex.ProviderID:
		return w.startCodexOAuth(ctx, proxyURL)
	case copilotProviderID:
		return w.startCopilotOAuth(ctx, proxyURL)
	default:
		return workspace.OAuthStartResult{}, nil, fmt.Errorf("oauth: unsupported provider %q", providerID)
	}
}

// CompleteOAuth implements workspace.OAuthController.
func (w *AppWorkspace) CompleteOAuth(ctx context.Context, providerID, proxyURL string, token *oauth.Token, forceNewAccount bool) (workspace.OAuthCompletion, error) {
	switch providerID {
	case codex.ProviderID:
		return w.completeCodexOAuth(ctx, proxyURL, token, forceNewAccount)
	case copilotProviderID:
		return w.completeCopilotOAuth(proxyURL, token, forceNewAccount)
	default:
		return workspace.OAuthCompletion{}, fmt.Errorf("oauth: unsupported provider %q", providerID)
	}
}

// OAuthConfiguredProxy implements workspace.OAuthController.
func (w *AppWorkspace) OAuthConfiguredProxy(providerID string) string {
	switch providerID {
	case codex.ProviderID:
		// What Sennit is already configured with wins; the Codex CLI's own
		// config is only a fallback prefill for someone who has never
		// configured Sennit's proxy but has told the CLI about theirs.
		if cfg := w.store.Config(); cfg != nil {
			if pc, ok := cfg.Providers.Get(codex.ProviderID); ok && pc.ProxyURL != "" {
				return pc.ProxyURL
			}
		}
		return codex.ProxyFromDisk()
	case copilotProviderID:
		// No sibling CLI to fall back to, matching the dialog's
		// configuredCopilotProxy this replaces.
		if cfg := w.store.Config(); cfg != nil {
			if pc, ok := cfg.Providers.Get(copilotProviderID); ok {
				return pc.ProxyURL
			}
		}
		return ""
	default:
		return ""
	}
}

// OAuthValidateProxy implements workspace.OAuthController.
//
// Both providers share proxyhttp.ValidateProxy (codex.ValidateProxy is
// just that check under an alias — see internal/oauth/codex/http.go) since
// the check itself is provider-neutral; Copilot has no dialog proxy step
// today, so this is exercised for it only defensively.
func (w *AppWorkspace) OAuthValidateProxy(providerID, proxyURL string) error {
	return codex.ValidateProxy(proxyURL)
}

// -- Codex --

// codexFlowAdapter wraps a *codex.Flow to satisfy workspace.OAuthFlow.
type codexFlowAdapter struct {
	flow *codex.Flow
}

func (a *codexFlowAdapter) Wait(ctx context.Context) (*oauth.Token, error) {
	return a.flow.Wait(ctx)
}

// Cancel is best-effort and safe to call once, mirroring the flow.Close()
// call sites it replaces (initiateAuth's early-teardown path in the old
// dialog, and login_codex.go's defer).
func (a *codexFlowAdapter) Cancel() {
	_ = a.flow.Close()
}

// startCodexOAuth reimplements the disk-reuse/refresh-then-browser-flow
// dance formerly duplicated between oauth_codex.go's initiateAuth and
// login_codex.go's codexToken.
func (w *AppWorkspace) startCodexOAuth(ctx context.Context, proxyURL string) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	if disk, ok := codex.TokensFromDisk(); ok {
		// Prefer the access token the CLI already holds: refreshing spends
		// its single-use refresh token and logs it out, which is not
		// something to do to another tool in passing.
		if token, ok := disk.Token(); ok {
			return workspace.OAuthStartResult{Token: token, ReusedExistingLogin: true}, nil, nil
		}

		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		token, err := codex.RefreshToken(refreshCtx, proxyURL, disk.RefreshToken)
		cancel()
		if err == nil {
			return workspace.OAuthStartResult{Token: token, RefreshedExistingLogin: true}, nil, nil
		}
		// A stale login on disk is not an error worth failing on: the
		// browser flow below is the fallback for exactly that.
		result, flow, flowErr := w.startCodexBrowserFlow(proxyURL)
		if flowErr != nil {
			return workspace.OAuthStartResult{}, nil, flowErr
		}
		result.ExistingLoginFailure = err.Error()
		return result, flow, nil
	}

	return w.startCodexBrowserFlow(proxyURL)
}

// startCodexBrowserFlow is split out of startCodexOAuth so both the
// no-disk-login and the stale-disk-login paths can build the
// browser-flow OAuthFlow the same way.
func (w *AppWorkspace) startCodexBrowserFlow(proxyURL string) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	flow, err := codex.StartFlow(proxyURL)
	if err != nil {
		return workspace.OAuthStartResult{}, nil, err
	}
	return workspace.OAuthStartResult{
		AuthorizationURL: flow.URL(),
	}, &codexFlowAdapter{flow: flow}, nil
}

// completeCodexOAuth records the account, then persists the proxy this
// sign-in used and fetches the account's model list. Both of those are
// best-effort: the credential is already the thing that matters, so a
// failure in either is reported back on OAuthCompletion (ProxyError,
// ModelsError) rather than failing the whole call — callers decide for
// themselves whether that makes the sign-in a failure (see the doc
// comments on those fields, and login_codex.go / oauth.go's saveCredential
// for the two different answers).
//
// The proxy write itself is skipped entirely, not merely retried with the
// same value, when proxyURL already matches what is configured: proxyURL
// may be a resolved value (e.g. pulled from codex.ProxyFromDisk) while the
// configured one is still an unresolved "$VAR" template, and writing the
// resolved form back would permanently replace the template on every
// sign-in that never asked for a proxy change at all. This is ported from
// the CLI's old loginCodex/restoreCodexProxyField, which guarded the same
// write the same way.
func (w *AppWorkspace) completeCodexOAuth(ctx context.Context, proxyURL string, token *oauth.Token, forceNewAccount bool) (workspace.OAuthCompletion, error) {
	// previousProxyURL is read straight off the config file rather than
	// the store's in-memory Config(): a provider with no credentials yet
	// is dropped from the published config entirely (see the config
	// loader's "missing api_key" validation, which is exactly the state
	// of a fresh sign-in's provider entry before RecordAccount runs), and
	// separately, ConfigStore's autoReload can fail — leaving Config()
	// stale, pointed at whatever the last successful reload saw — when it
	// cannot resolve a default model, which is the state of any provider
	// with no models configured yet. Either way, cfg.Providers.Get would
	// silently answer "not configured" for a proxy that is, in fact, on
	// disk. Reading the file is what a rewrite would actually clobber, so
	// it is what the guard below must compare against.
	previousProxyURL := previousCodexProxyURL(w)

	accountID := codex.AccountID(token.AccessToken)
	account, err := w.RecordAccount(config.ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:           token,
		AccountID:       accountID,
		Email:           codex.Email(token.AccessToken),
		ForceNewAccount: forceNewAccount,
	})
	if err != nil {
		return workspace.OAuthCompletion{}, err
	}
	completion := workspace.OAuthCompletion{Account: account}

	if proxyURL != previousProxyURL {
		proxyKey := "providers." + codex.ProviderID + ".proxy_url"
		var proxyErr error
		if proxyURL == "" {
			proxyErr = removeCodexProxyConfigField(w, config.ScopeGlobal, proxyKey)
		} else {
			proxyErr = setCodexProxyConfigField(w, config.ScopeGlobal, proxyKey, proxyURL)
		}
		if proxyErr != nil {
			completion.ProxyError = fmt.Errorf("signed in, but the proxy setting could not be saved: %w", proxyErr)
		}
	}

	// Model fetching runs on its own timeout rather than ctx's deadline:
	// the token exchange/refresh above is already done, and binding this
	// to a caller context that may be tied to a program's shutdown would
	// mean a shutdown landing in this narrow window silently drops a
	// completed sign-in's model list, leaving a saved credential with no
	// models to show for it. context.WithoutCancel(ctx) keeps whatever
	// values ctx carries while dropping its cancellation — mirrors
	// internal/agent/tools/mcp/connection.go's createSession.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	models, err := codex.FetchModels(fetchCtx, proxyURL, token.AccessToken, accountID)
	if err != nil {
		completion.ModelsError = fmt.Errorf("signed in, but the model list could not be fetched: %w", err)
		return completion, nil
	}
	if err := w.SetConfigField(config.ScopeGlobal, "providers."+codex.ProviderID+".models", models); err != nil {
		completion.ModelsError = err
		return completion, nil
	}
	completion.ModelsFetched = len(models)
	return completion, nil
}

// -- Copilot --

// copilotFlowAdapter wraps copilot.PollForToken to satisfy
// workspace.OAuthFlow.
type copilotFlowAdapter struct {
	proxyURL string
	device   *copilot.DeviceCode
}

func (a *copilotFlowAdapter) Wait(ctx context.Context) (*oauth.Token, error) {
	return copilot.PollForToken(ctx, a.proxyURL, a.device)
}

// Cancel is a no-op: PollForToken has nothing to release beyond what
// cancelling the ctx passed to Wait already does. Callers still call it
// unconditionally, matching OAuthFlow's contract.
func (a *copilotFlowAdapter) Cancel() {}

func (w *AppWorkspace) startCopilotOAuth(ctx context.Context, proxyURL string) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	device, err := copilot.RequestDeviceCode(requestCtx, proxyURL)
	if err != nil {
		return workspace.OAuthStartResult{}, nil, fmt.Errorf("failed to initiate device auth: %w", err)
	}
	return workspace.OAuthStartResult{
		DeviceCode:      device.DeviceCode,
		UserCode:        device.UserCode,
		VerificationURL: device.VerificationURI,
		ExpiresIn:       device.ExpiresIn,
		Interval:        device.Interval,
	}, &copilotFlowAdapter{proxyURL: proxyURL, device: device}, nil
}

func (w *AppWorkspace) completeCopilotOAuth(_ string, token *oauth.Token, forceNewAccount bool) (workspace.OAuthCompletion, error) {
	// Copilot has no way to derive an account identity or email from the
	// token, matching today's dialog (OAuthCopilot implements neither
	// oauthAccountIDer nor oauthAccountEmailer nor oauthPostSaver).
	account, err := w.RecordAccount(config.ScopeGlobal, copilotProviderID, accounts.LegacyCredential{
		Token:           token,
		ForceNewAccount: forceNewAccount,
	})
	if err != nil {
		return workspace.OAuthCompletion{}, err
	}
	return workspace.OAuthCompletion{Account: account, ModelsFetched: -1}, nil
}
