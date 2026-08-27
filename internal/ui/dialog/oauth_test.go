package dialog

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// fakeCodexJWT builds an unsigned token carrying the chatgpt_account_id
// claim OAuthCodex.accountID reads, mirroring
// internal/oauth/codex/codex_test.go's fakeJWT — nothing here verifies
// signatures, since the claims are read out of a token the authorization
// server already issued.
func fakeCodexJWT(t *testing.T, accountID string) string {
	t.Helper()
	claims := map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
		"exp":                         time.Now().Add(10 * 24 * time.Hour).Unix(),
	}
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// recordAccountTestWorkspace is a minimal [workspace.Workspace] stub that
// records RecordAccount calls, for proving saveCredential records an
// account rather than overwriting the single credential, and that it does
// so off the Update loop.
type recordAccountTestWorkspace struct {
	workspace.Workspace

	recordCalls    int
	lastScope      config.Scope
	lastProviderID string
	lastCred       accounts.LegacyCredential
	recordErr      error
}

func (w *recordAccountTestWorkspace) RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	w.recordCalls++
	w.lastScope = scope
	w.lastProviderID = providerID
	w.lastCred = cred
	return accounts.Account{ID: "acct-1"}, w.recordErr
}

// stubAccountIDProvider is a stub OAuthProvider implementing only the
// optional oauthAccountIDer half (not oauthPostSaver), so saveCredential's
// use of the derived account ID can be tested without triggering a real
// provider's post-save network calls (e.g. OAuthCodex.afterSave's model
// fetch).
type stubAccountIDProvider struct {
	stubOAuthProvider
	id string
}

func (s *stubAccountIDProvider) accountID(*oauth.Token) string { return s.id }

var _ oauthAccountIDer = (*stubAccountIDProvider)(nil)

// oauthSaveDoneMsgFilter matches the message saveCredential's command
// produces once the account is recorded (and any post-save work finishes).
func oauthSaveDoneMsgFilter(msg tea.Msg) bool {
	_, ok := msg.(oauthSaveDoneMsg)
	return ok
}

// TestOAuthSaveCredential_RecordsAccount_NoIOInHandleMsg is the
// HandleMsg-does-no-IO regression test for saveCredential: it must record
// the account via a [tea.Cmd], not synchronously, and — for a provider that
// implements neither optional half of [OAuthProvider] — record with no
// account ID and no proxy of its own.
func TestOAuthSaveCredential_RecordsAccount_NoIOInHandleMsg(t *testing.T) {
	s := styles.SennitDark()
	provider := catwalk.Provider{ID: catwalk.InferenceProviderOpenAI, Name: "OpenAI"}
	ws := &recordAccountTestWorkspace{}
	com := &common.Common{Styles: &s, Workspace: ws}

	stub := &stubOAuthProvider{}
	dlg, _ := newOAuth(com, false, provider, nil, stub, false)

	token := &oauth.Token{AccessToken: "tok-123"}
	action := dlg.HandleMsg(ActionCompleteOAuth{Token: token})
	require.Zero(t, ws.recordCalls, "HandleMsg must not record the account synchronously")

	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected ActionCmd carrying the async save, got %#v", action)

	msg := findMsg(t, cmdAction.Cmd, oauthSaveDoneMsgFilter)
	require.NotNil(t, msg, "expected oauthSaveDoneMsg once saveCredential's command runs")

	require.Equal(t, 1, ws.recordCalls)
	require.Equal(t, config.ScopeGlobal, ws.lastScope)
	require.Equal(t, string(provider.ID), ws.lastProviderID)
	require.Equal(t, token, ws.lastCred.Token)
	require.Empty(t, ws.lastCred.AccountID, "a provider without oauthAccountIDer must record no account ID")
	require.Empty(t, ws.lastCred.ProxyURL, "the account itself carries no proxy; see saveCredential's comment")
}

// TestOAuthSaveCredential_UsesOptionalAccountID covers the other half: a
// provider implementing oauthAccountIDer has its derived ID carried
// through to the recorded [accounts.LegacyCredential].
func TestOAuthSaveCredential_UsesOptionalAccountID(t *testing.T) {
	s := styles.SennitDark()
	provider := catwalk.Provider{ID: catwalk.InferenceProviderOpenAI, Name: "Codex-like"}
	ws := &recordAccountTestWorkspace{}
	com := &common.Common{Styles: &s, Workspace: ws}

	stub := &stubAccountIDProvider{id: "acct-codex-1"}
	dlg, _ := newOAuth(com, false, provider, nil, stub, false)

	token := &oauth.Token{AccessToken: "tok-xyz"}
	action := dlg.HandleMsg(ActionCompleteOAuth{Token: token})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok)

	msg := findMsg(t, cmdAction.Cmd, oauthSaveDoneMsgFilter)
	require.NotNil(t, msg)

	require.Equal(t, 1, ws.recordCalls)
	require.Equal(t, "acct-codex-1", ws.lastCred.AccountID)
}

// TestOAuthSaveCredential_ThreadsForceNewAccount covers a dialog session
// started as a deliberate "Add account…" sign-in: saveCredential must
// carry that intent through to RecordAccount as
// accounts.LegacyCredential.ForceNewAccount, so a provider with no
// account identity of its own creates a new account instead of updating
// the active one in place.
func TestOAuthSaveCredential_ThreadsForceNewAccount(t *testing.T) {
	s := styles.SennitDark()
	provider := catwalk.Provider{ID: catwalk.InferenceProviderOpenAI, Name: "OpenAI"}
	ws := &recordAccountTestWorkspace{}
	com := &common.Common{Styles: &s, Workspace: ws}

	stub := &stubOAuthProvider{}
	dlg, _ := newOAuth(com, false, provider, nil, stub, true)

	token := &oauth.Token{AccessToken: "tok-force"}
	action := dlg.HandleMsg(ActionCompleteOAuth{Token: token})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok)

	msg := findMsg(t, cmdAction.Cmd, oauthSaveDoneMsgFilter)
	require.NotNil(t, msg)

	require.Equal(t, 1, ws.recordCalls)
	require.True(t, ws.lastCred.ForceNewAccount, "an explicit add-account session must set ForceNewAccount")
}

// TestOAuthCodex_AccountIDDerivedFromToken pins OAuthCodex's half of
// oauthAccountIDer: the account identity comes from the access token's JWT
// claim via codex.AccountID, matching internal/oauth/codex's own tests.
func TestOAuthCodex_AccountIDDerivedFromToken(t *testing.T) {
	token := &oauth.Token{AccessToken: fakeCodexJWT(t, "acct-jwt-1")}
	m := &OAuthCodex{}
	require.Equal(t, "acct-jwt-1", m.accountID(token))
}

// stubOAuthProvider is a minimal OAuthProvider that records whether
// stopPolling was invoked, for the esc-during-polling regression test.
type stubOAuthProvider struct {
	stopPollingCalls int
}

func (s *stubOAuthProvider) name() string                     { return "stub" }
func (s *stubOAuthProvider) initiateAuth() tea.Msg            { return nil }
func (s *stubOAuthProvider) startPolling(string, int) tea.Cmd { return nil }
func (s *stubOAuthProvider) stopPolling() tea.Msg {
	s.stopPollingCalls++
	return nil
}

var _ OAuthProvider = (*stubOAuthProvider)(nil)

// TestOAuth_WithoutModelReturnsActionProviderConfigured covers the
// model-less mode (used by the providers-configuration dialog): with a nil
// model, confirming a successful OAuth flow produces ActionProviderConfigured
// instead of ActionSelectModel.
func TestOAuth_WithoutModelReturnsActionProviderConfigured(t *testing.T) {
	s := styles.SennitDark()
	com := &common.Common{Styles: &s}

	provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
	dlg, _ := NewOAuthCopilot(com, false, provider, nil, false)

	dlg.State = OAuthStateSuccess

	action := dlg.confirmAndSelectModel()
	configuredAction, ok := action.(ActionProviderConfigured)
	require.True(t, ok, "expected ActionProviderConfigured, got %#v", action)
	require.Equal(t, string(provider.ID), configuredAction.ProviderID)
}

// TestOAuthEscStopsPollingBeforeClosing is the regression test for the leak
// where esc during OAuthStateDisplay/OAuthStateInitializing closed the
// dialog without stopping polling: the Copilot flow kept polling GitHub
// until expiresIn, and the Codex flow kept its loopback listener bound so a
// second sign-in attempt failed to bind the port.
func TestOAuthEscStopsPollingBeforeClosing(t *testing.T) {
	states := map[string]OAuthState{
		"initializing": OAuthStateInitializing,
		"display":      OAuthStateDisplay,
	}
	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			s := styles.SennitDark()
			com := &common.Common{Styles: &s}
			provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
			stub := &stubOAuthProvider{}
			dlg, _ := newOAuth(com, false, provider, nil, stub, false)
			dlg.State = state

			action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEsc})

			batch, ok := action.(ActionBatch)
			require.True(t, ok, "expected ActionBatch{ActionCmd, ActionClose}, got %#v", action)

			var sawClose, ranCmd bool
			for _, a := range batch.Actions {
				switch a := a.(type) {
				case ActionClose:
					sawClose = true
				case ActionCmd:
					require.NotNil(t, a.Cmd)
					a.Cmd()
					ranCmd = true
				}
			}
			require.True(t, sawClose, "esc must still close the dialog")
			require.True(t, ranCmd)
			require.Equal(t, 1, stub.stopPollingCalls)
		})
	}
}
