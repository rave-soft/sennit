package dialog

import (
	"context"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
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
//
// It is spelled out rather than aliased to codex.ProviderID because the
// sign-in flow itself now lives behind the workspace (see
// workspace.OAuthController), and this package no longer imports the
// Codex vendor package at all. The two must stay in lockstep.
const CodexProviderID = "codex"

// codexProviderName is how the provider is shown in the dialog, matching
// codex.ProviderName for the same reason CodexProviderID is spelled out.
const codexProviderName = "OpenAI Codex"

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
	// binds a fixed callback port, and a teardown that runs before the
	// bind finishes would otherwise find nothing to close — the port
	// would stay bound and the next sign-in would fail on it. A flow
	// that lands after the teardown is closed immediately instead.
	mu         sync.Mutex
	flow       workspace.OAuthFlow
	cancelFunc func()
	stopped    bool
}

var (
	_ OAuthProvider        = (*OAuthCodex)(nil)
	_ oauthProxyConfigurer = (*OAuthCodex)(nil)
	_ oauthProxyUser       = (*OAuthCodex)(nil)
)

func (m *OAuthCodex) name() string {
	return codexProviderName
}

// currentProxy implements [oauthProxyUser]: the proxy this sign-in used has
// to reach CompleteOAuth, which persists it as the provider's default and
// routes the model-list fetch through it.
func (m *OAuthCodex) currentProxy() string {
	return m.proxy
}

// proxyURL prefills the step with whatever the backend says the provider
// already uses — Sennit's own configuration, falling back to the Codex
// CLI's. Someone behind a proxy has told the CLI about it already, and
// asking again for the same fact is a worse first impression than offering
// it back for confirmation.
func (m *OAuthCodex) proxyURL() string {
	if m.proxy != "" {
		return m.proxy
	}
	// Common carries no workspace in tests, so its absence is a
	// legitimate "nothing configured" here.
	if m.com == nil || m.com.Workspace == nil {
		return ""
	}
	return m.com.Workspace.OAuthConfiguredProxy(CodexProviderID)
}

// setProxyURL validates the entered value up front: a bad proxy would
// otherwise surface as a confusing sign-in failure a few seconds later.
func (m *OAuthCodex) setProxyURL(proxyURL string) error {
	if m.com != nil && m.com.Workspace != nil {
		if err := m.com.Workspace.OAuthValidateProxy(CodexProviderID, proxyURL); err != nil {
			return err
		}
	}
	m.proxy = proxyURL
	return nil
}

// initiateAuth starts the flow, which binds the callback port before
// returning so the URL cannot be opened ahead of anything listening for the
// redirect.
//
// An existing Codex CLI login short-circuits the browser entirely: the
// backend reports the token it could reuse or refresh instead of a URL.
func (m *OAuthCodex) initiateAuth() tea.Msg {
	result, flow, err := m.com.Workspace.StartOAuth(m.com.Context(), CodexProviderID, m.proxy)
	if err != nil {
		return ActionOAuthErrored{Error: err}
	}
	if result.Token != nil {
		return ActionCompleteOAuth{Token: result.Token}
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		// Dismissed while this was binding: release the port rather than
		// leaving it held by a dialog that is gone.
		if flow != nil {
			flow.Cancel()
		}
		return nil
	}
	m.flow = flow
	m.mu.Unlock()

	return ActionInitiateOAuth{
		VerificationURL: result.AuthorizationURL,
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
		flow.Cancel()
	}
	return nil
}
