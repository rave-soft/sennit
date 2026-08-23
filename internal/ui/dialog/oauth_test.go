package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

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
	dlg, _ := NewOAuthCopilot(com, false, provider, nil)

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
			dlg, _ := newOAuth(com, false, provider, nil, stub)
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
