package dialog

import (
	"context"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/workspace"
)

// codexAuthTimeout bounds how long the dialog waits for the browser half of
// the sign-in before giving up and freeing the callback port.
const codexAuthTimeout = 10 * time.Minute

// CodexProviderID is the provider this dialog signs in to. It is exported
// so the caller that picks a sign-in dialog can name the provider without
// importing the vendor package: the knowledge "this id means Codex"
// belongs next to the dialog that handles it, and internal/ui/model has no
// other reason to reach into a package that carries an OAuth flow.
const CodexProviderID = codex.ProviderID

// NewOAuthCodex builds the Codex sign-in dialog.
//
// Codex is a redirect flow, not a device flow: there is no code to type, so
// the dialog shows only the URL and waits for the browser to come back to
// the loopback listener the flow owns. It also opens on a proxy step, since
// for many users the endpoint is only reachable through one.
func NewOAuthCodex(
	com *common.Common,
	isOnboarding bool,
	provider catwalk.Provider,
	model *config.SelectedModel,
	forceNewAccount bool,
) (*OAuth, tea.Cmd) {
	return newOAuth(com, isOnboarding, provider, model, &OAuthCodex{com: com}, forceNewAccount)
}

type OAuthCodex struct {
	com   *common.Common
	proxy string

	// flow/cancelFunc/stopped are touched from tea.Cmd goroutines
	// (initiateAuth binds the flow, startPolling waits on it, stopPolling
	// tears it down) rather than from Update, so they need a lock of
	// their own.
	//
	// stopped is what makes an esc during "Initializing" safe: the flow
	// binds a fixed callback port, and a teardown that ran before the
	// bind finished used to find nothing to close — the port stayed bound
	// and the next sign-in failed on it. A flow that lands after the
	// teardown is closed immediately instead.
	mu         sync.Mutex
	flow       *codex.Flow
	cancelFunc func()
	stopped    bool
}

var (
	_ OAuthProvider        = (*OAuthCodex)(nil)
	_ oauthPostSaver       = (*OAuthCodex)(nil)
	_ oauthProxyConfigurer = (*OAuthCodex)(nil)
	_ oauthAccountIDer     = (*OAuthCodex)(nil)
	_ oauthAccountEmailer  = (*OAuthCodex)(nil)
)

func (m *OAuthCodex) name() string {
	return codex.ProviderName
}

// accountID implements [oauthAccountIDer]: Codex embeds the account
// identity in the access token's JWT claims.
func (m *OAuthCodex) accountID(token *oauth.Token) string {
	return codex.AccountID(token.AccessToken)
}

// accountEmail implements [oauthAccountEmailer]: the same token carries
// the signed-in address, which is what the accounts list shows instead of
// accountID's UUID.
func (m *OAuthCodex) accountEmail(token *oauth.Token) string {
	return codex.Email(token.AccessToken)
}

// proxyURL prefills the step, in order of how much it is known to be what
// the user wants: what Sennit is already configured with, then what the
// Codex CLI on this machine uses. Someone behind a proxy has told the CLI
// about it already, and asking again for the same fact is a worse first
// impression than offering it back for confirmation.
func (m *OAuthCodex) proxyURL() string {
	if m.proxy != "" {
		return m.proxy
	}
	// Common carries no workspace in tests, and Config panics on one, so
	// the absence of a config is a legitimate "nothing configured" here.
	if m.com != nil && m.com.Workspace != nil {
		if cfg := m.com.Config(); cfg != nil {
			if pc, ok := cfg.Providers.Get(codex.ProviderID); ok && pc.ProxyURL != "" {
				return pc.ProxyURL
			}
		}
	}
	return codex.ProxyFromDisk()
}

// setProxyURL validates the entered value up front: a bad proxy would
// otherwise surface as a confusing sign-in failure a few seconds later.
func (m *OAuthCodex) setProxyURL(proxyURL string) error {
	if err := codex.ValidateProxy(proxyURL); err != nil {
		return err
	}
	m.proxy = proxyURL
	return nil
}

// initiateAuth starts the flow, which binds the callback port before
// returning so the URL cannot be opened ahead of anything listening for the
// redirect.
//
// An existing Codex CLI login short-circuits the browser entirely: its
// refresh token is exchanged for our own pair.
func (m *OAuthCodex) initiateAuth() tea.Msg {
	if disk, ok := codex.TokensFromDisk(); ok {
		// Prefer the access token the CLI already holds: refreshing spends
		// its single-use refresh token and logs it out, which is not
		// something to do to another tool in passing.
		if token, ok := disk.Token(); ok {
			return ActionCompleteOAuth{Token: token}
		}

		ctx, cancel := context.WithTimeout(m.com.Context(), 30*time.Second)
		defer cancel()
		if token, err := codex.RefreshToken(ctx, m.proxy, disk.RefreshToken); err == nil {
			return ActionCompleteOAuth{Token: token}
		}
		// A stale login on disk is not an error worth showing: the browser
		// flow below is the fallback for exactly that.
	}

	flow, err := codex.StartFlow(m.proxy)
	if err != nil {
		return ActionOAuthErrored{Error: err}
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		// Dismissed while this was binding: release the port rather than
		// leaving it held by a dialog that is gone.
		_ = flow.Close()
		return nil
	}
	m.flow = flow
	m.mu.Unlock()

	return ActionInitiateOAuth{
		VerificationURL: flow.URL(),
		ExpiresIn:       int(codexAuthTimeout.Seconds()),
	}
}

// startPolling waits for the redirect. Nothing is actually polled — the
// browser comes to us — but the shared dialog drives every flow through this
// one hook.
func (m *OAuthCodex) startPolling(_ string, _ int) tea.Cmd {
	m.mu.Lock()
	flow := m.flow
	if flow == nil {
		m.mu.Unlock()
		return func() tea.Msg {
			return ActionOAuthErrored{Error: fmt.Errorf("codex sign-in was not started")}
		}
	}
	ctx, cancel := context.WithTimeout(m.com.Context(), codexAuthTimeout)
	m.cancelFunc = cancel
	m.mu.Unlock()
	return func() tea.Msg {
		token, err := flow.Wait(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // cancelled or timed out; the dialog is gone.
			}
			return ActionOAuthErrored{Error: err}
		}
		return ActionCompleteOAuth{Token: token}
	}
}

// stopPolling releases the callback port, whether the sign-in succeeded,
// failed, or was dismissed. Leaving it bound would break the next attempt,
// since the redirect URI names one fixed port.
func (m *OAuthCodex) stopPolling() tea.Msg {
	m.mu.Lock()
	m.stopped = true
	cancel, flow := m.cancelFunc, m.flow
	m.cancelFunc, m.flow = nil, nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if flow != nil {
		_ = flow.Close()
	}
	return nil
}

// afterSave stores the proxy the sign-in used and the model list for the
// account that just signed in.
//
// The proxy is written first and unconditionally: model requests have to go
// the same way the sign-in did, and clearing an emptied field matters as
// much as saving a new one.
func (m *OAuthCodex) afterSave(ws workspace.ConfigFieldEditor, token *oauth.Token) error {
	proxyKey := "providers." + codex.ProviderID + ".proxy_url"
	if m.proxy == "" {
		if err := ws.RemoveConfigField(config.ScopeGlobal, proxyKey); err != nil {
			return err
		}
	} else if err := ws.SetConfigField(config.ScopeGlobal, proxyKey, m.proxy); err != nil {
		return err
	}

	// afterSave runs after the token is already good and the browser half
	// of the flow is done — it only persists what was won. Binding this to
	// the program's lifecycle context would mean a shutdown that lands in
	// this narrow window silently drops a completed sign-in's model list,
	// leaving a saved credential with no models to show for it. A fresh
	// context lets the save finish regardless.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) //nolint:forbidigo // post-auth persistence must outlive shutdown; see comment above
	defer cancel()

	models, err := codex.FetchModels(ctx, m.proxy, token.AccessToken, codex.AccountID(token.AccessToken))
	if err != nil {
		return fmt.Errorf("signed in, but the Codex model list could not be fetched: %w", err)
	}
	return ws.SetConfigField(config.ScopeGlobal, "providers."+codex.ProviderID+".models", models)
}
