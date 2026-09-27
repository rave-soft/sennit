package dialog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// errBrowserOpenTest stands in for whatever browser.OpenURL would return
// on failure, for TestOAuthBrowserOpenFailure_DoesNotAbortSignIn.
var errBrowserOpenTest = errors.New("no browser launcher available")

// completeOAuthTestWorkspace is a minimal [workspace.Workspace] stub —
// mirroring accountsTestWorkspace's comment on why it must embed the full
// interface — standing in for the backend that now completes a sign-in
// (persisting the account, fetching models, saving the proxy) behind
// StartOAuth/OAuthFlow.Wait instead of a separate CompleteOAuth step (see
// CLIENT-SERVER.md PR 1.3).
type completeOAuthTestWorkspace struct {
	workspace.Workspace

	// mu guards the StartOAuth bookkeeping below: initiateAuth runs off a
	// tea.Cmd goroutine, so a test asserting on it races the dialog
	// otherwise.
	mu                  sync.Mutex
	startCalls          int
	lastStartProviderID string
	lastStartProxy      string
	startResult         workspace.OAuthStartResult
	lastStartForceNew   bool
	startFlow           *stubDialogOAuthFlow
	startErr            error

	configuredProxy string
}

func (w *completeOAuthTestWorkspace) OAuthConfiguredProxy(string) string { return w.configuredProxy }

// OAuthValidateProxy stands in for the backend's provider-neutral check
// (proxyhttp.ValidateProxy): only the schemes it accepts pass.
func (w *completeOAuthTestWorkspace) OAuthValidateProxy(_, proxyURL string) error {
	if proxyURL == "" {
		return nil
	}
	for _, scheme := range []string{"http://", "https://", "socks5://", "socks5h://"} {
		if strings.HasPrefix(proxyURL, scheme) {
			return nil
		}
	}
	return fmt.Errorf("invalid proxy_url %q", proxyURL)
}

// StartOAuth on completeOAuthTestWorkspace hands back whatever the test
// staged: a completed sign-in with nothing to show, or a flow to wait on.
func (w *completeOAuthTestWorkspace) StartOAuth(_ context.Context, providerID, proxyURL string, forceNewAccount bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.startCalls++
	w.lastStartProviderID = providerID
	w.lastStartProxy = proxyURL
	w.lastStartForceNew = forceNewAccount
	if w.startErr != nil {
		return workspace.OAuthStartResult{}, nil, w.startErr
	}
	if w.startFlow == nil {
		return w.startResult, nil, nil
	}
	return w.startResult, w.startFlow, nil
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

// TestOAuthCompleteOAuth_SuccessScreen_NoIOInHandleMsg pins that HandleMsg
// itself never touches the workspace: by the time ActionCompleteOAuth
// arrives, the backend has already done everything (see
// workspace.OAuthFlow.Wait), so HandleMsg only has to read the completion
// it was handed.
func TestOAuthCompleteOAuth_SuccessScreen_NoIOInHandleMsg(t *testing.T) {
	s := styles.SennitDark()
	provider := catwalk.Provider{ID: catwalk.InferenceProviderOpenAI, Name: "OpenAI"}
	ws := &completeOAuthTestWorkspace{}
	com := &common.Common{Styles: &s, Workspace: ws}

	stub := &stubOAuthProvider{}
	dlg, _ := newOAuth(com, false, provider, nil, stub, false)

	completion := workspace.OAuthCompletion{
		Account: workspace.FrontendAccount{Email: "someone@example.com"},
	}
	action := dlg.HandleMsg(ActionCompleteOAuth{Completion: completion})

	require.Equal(t, OAuthStateSuccess, dlg.State)
	require.Equal(t, "someone@example.com", dlg.signedInAs)

	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected an ActionCmd stopping polling, got %#v", action)
	require.NotNil(t, cmdAction.Cmd)
	cmdAction.Cmd()
	require.Equal(t, 1, stub.stopPollingCalls)
}

// TestOAuthCompleteOAuth_ModelFetchFailureFailsTheDialog pins the dialog's
// half of OAuthCompletion.ModelsError: the credential is saved, but a
// sign-in that leaves nothing to select is an error here (unlike the CLI,
// which reports it as a warning and exits successfully).
func TestOAuthCompleteOAuth_ModelFetchFailureFailsTheDialog(t *testing.T) {
	s := styles.SennitDark()
	provider := catwalk.Provider{ID: catwalk.InferenceProviderOpenAI, Name: "OpenAI"}
	com := &common.Common{Styles: &s}

	dlg, _ := newOAuth(com, false, provider, nil, &stubOAuthProvider{}, false)

	completion := workspace.OAuthCompletion{ModelsError: workspace.EncodeError(errors.New("model list unavailable"))}
	action := dlg.HandleMsg(ActionCompleteOAuth{Completion: completion})

	require.Equal(t, OAuthStateError, dlg.State)
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok)
	msg := findMsg(t, cmdAction.Cmd, func(msg tea.Msg) bool {
		_, ok := msg.(util.InfoMsg)
		return ok
	})
	require.NotNil(t, msg, "expected a reported error")
	warn := msg.(util.InfoMsg)
	require.Contains(t, warn.Msg, "model list unavailable")
}

// TestOAuthCompleteOAuth_ProxyWriteFailureFailsTheDialog pins the dialog's
// treatment of OAuthCompletion.ProxyError: like ModelsError, the credential
// is already saved, but a proxy that could not be persisted is still an
// error here.
func TestOAuthCompleteOAuth_ProxyWriteFailureFailsTheDialog(t *testing.T) {
	s := styles.SennitDark()
	provider := catwalk.Provider{ID: catwalk.InferenceProviderOpenAI, Name: "OpenAI"}
	com := &common.Common{Styles: &s}

	dlg, _ := newOAuth(com, false, provider, nil, &stubOAuthProvider{}, false)

	completion := workspace.OAuthCompletion{ProxyError: workspace.EncodeError(errors.New("proxy setting unavailable"))}
	action := dlg.HandleMsg(ActionCompleteOAuth{Completion: completion})

	require.Equal(t, OAuthStateError, dlg.State)
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok)
	msg := findMsg(t, cmdAction.Cmd, func(msg tea.Msg) bool {
		_, ok := msg.(util.InfoMsg)
		return ok
	})
	require.NotNil(t, msg, "expected a reported error")
	warn := msg.(util.InfoMsg)
	require.Contains(t, warn.Msg, "proxy setting unavailable")
}

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

// TestOAuthBrowserOpenFailure_DoesNotAbortSignIn is the regression test
// for a failed browser.OpenURL aborting the whole device-flow sign-in:
// copyCodeAndOpenURL used to turn that failure into ActionOAuthErrored,
// which drops the dialog into OAuthStateError and stops polling — even
// though the code and verification URL are already on screen and the
// user can still finish sign-in by opening the URL themselves. The fix
// reports it as a warning instead and leaves state/polling untouched.
func TestOAuthBrowserOpenFailure_DoesNotAbortSignIn(t *testing.T) {
	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	provider := catwalk.Provider{ID: catwalk.InferenceProviderOpenAI, Name: "OpenAI"}
	stub := &stubOAuthProvider{}
	dlg, _ := newOAuth(com, false, provider, nil, stub, false)

	// Get into OAuthStateDisplay the same way a real flow does.
	dlg.HandleMsg(ActionInitiateOAuth{
		DeviceCode:      "dev",
		UserCode:        "ABCD-1234",
		VerificationURL: "https://example.com/activate",
		ExpiresIn:       600,
		Interval:        5,
	})
	require.Equal(t, OAuthStateDisplay, dlg.State)

	// Simulate what copyCodeAndOpenURL's command produces when
	// browser.OpenURL fails, without depending on an actual browser
	// launcher behaving predictably in a test/CI environment.
	action := dlg.HandleMsg(oauthBrowserOpenFailedMsg{err: errBrowserOpenTest})

	require.Equal(t, OAuthStateDisplay, dlg.State, "a browser-open failure must not abort the sign-in flow")
	require.Zero(t, stub.stopPollingCalls, "polling must keep running so the code can still be redeemed")

	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected an ActionCmd carrying the warning, got %#v", action)
	msg := cmdAction.Cmd()
	warn, ok := msg.(util.InfoMsg)
	require.True(t, ok, "expected an inline warning message, got %#v", msg)
	require.Equal(t, util.InfoTypeWarn, warn.Type)
	require.Contains(t, warn.Msg, "Open the URL")
}

// stubDialogOAuthFlow stands in for a started sign-in: Wait blocks until
// its context is done (or until a canned result is handed to it), which is
// what a real browser/device flow does while the dialog is open.
type stubDialogOAuthFlow struct {
	completion workspace.OAuthCompletion
	err        error
	// ready, when non-nil, gates Wait: it returns only once ready is
	// closed or ctx is done.
	ready chan struct{}

	mu        sync.Mutex
	cancelled int
}

func (f *stubDialogOAuthFlow) Wait(ctx context.Context) (workspace.OAuthCompletion, error) {
	if f.ready != nil {
		select {
		case <-f.ready:
		case <-ctx.Done():
			return workspace.OAuthCompletion{}, ctx.Err()
		}
	}
	if f.err == nil && f.completion.Account.ID == "" && f.completion.Account.Email == "" {
		<-ctx.Done()
		return workspace.OAuthCompletion{}, ctx.Err()
	}
	return f.completion, f.err
}

func (f *stubDialogOAuthFlow) Cancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled++
}

func (f *stubDialogOAuthFlow) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

// StartRelay is a no-op: these tests never reach a remote daemon, so
// there is nothing to relay (CLIENT-SERVER.md, PR 3.3).
func (f *stubDialogOAuthFlow) StartRelay(context.Context, string) (func(), error) {
	return func() {}, nil
}
