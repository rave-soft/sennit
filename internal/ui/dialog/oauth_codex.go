package dialog

import (
	"context"
	"fmt"
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

// NewOAuthCodex builds the Codex sign-in dialog.
//
// Codex is a redirect flow, not a device flow: there is no code to type, so
// the dialog shows only the URL and waits for the browser to come back to
// the loopback listener the flow owns.
func NewOAuthCodex(
	com *common.Common,
	isOnboarding bool,
	provider catwalk.Provider,
	model *config.SelectedModel,
) (*OAuth, tea.Cmd) {
	return newOAuth(com, isOnboarding, provider, model, &OAuthCodex{})
}

type OAuthCodex struct {
	flow       *codex.Flow
	cancelFunc func()
}

var (
	_ OAuthProvider  = (*OAuthCodex)(nil)
	_ oauthPostSaver = (*OAuthCodex)(nil)
)

func (m *OAuthCodex) name() string {
	return codex.ProviderName
}

// initiateAuth starts the flow, which binds the callback port before
// returning so the URL cannot be opened ahead of anything listening for the
// redirect.
//
// An existing Codex CLI login short-circuits the browser entirely: its
// refresh token is exchanged for our own pair.
func (m *OAuthCodex) initiateAuth() tea.Msg {
	if disk, ok := codex.TokensFromDisk(); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if token, err := codex.RefreshToken(ctx, disk.RefreshToken); err == nil {
			return ActionCompleteOAuth{Token: token}
		}
		// A stale login on disk is not an error worth showing: the browser
		// flow below is the fallback for exactly that.
	}

	flow, err := codex.StartFlow()
	if err != nil {
		return ActionOAuthErrored{Error: err}
	}
	m.flow = flow

	return ActionInitiateOAuth{
		VerificationURL: flow.URL(),
		ExpiresIn:       int(codexAuthTimeout.Seconds()),
	}
}

// startPolling waits for the redirect. Nothing is actually polled — the
// browser comes to us — but the shared dialog drives every flow through this
// one hook.
func (m *OAuthCodex) startPolling(_ string, _ int) tea.Cmd {
	return func() tea.Msg {
		if m.flow == nil {
			return ActionOAuthErrored{Error: fmt.Errorf("codex sign-in was not started")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), codexAuthTimeout)
		m.cancelFunc = cancel

		token, err := m.flow.Wait(ctx)
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
	if m.cancelFunc != nil {
		m.cancelFunc()
		m.cancelFunc = nil
	}
	if m.flow != nil {
		_ = m.flow.Close()
		m.flow = nil
	}
	return nil
}

// afterSave stores the model list for the account that just signed in. The
// catalog entry ships without models because which ones an account may use
// depends on its plan.
func (m *OAuthCodex) afterSave(ws workspace.Workspace, token *oauth.Token) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	models, err := codex.FetchModels(ctx, token.AccessToken, codex.AccountID(token.AccessToken))
	if err != nil {
		return fmt.Errorf("signed in, but the Codex model list could not be fetched: %w", err)
	}
	return ws.SetConfigField(config.ScopeGlobal, "providers."+codex.ProviderID+".models", models)
}
