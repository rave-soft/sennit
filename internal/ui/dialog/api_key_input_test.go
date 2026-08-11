package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// apiKeyTestWorkspace is a minimal [workspace.Workspace] stub: it must
// embed the full interface (see testWorkspace in internal/ui/model/ui_test.go
// for the rationale) even though these tests only exercise
// SetProviderAPIKey.
type apiKeyTestWorkspace struct {
	workspace.Workspace
	savedProviderID string
	savedAPIKey     string
}

func (w *apiKeyTestWorkspace) SupportsThreads() bool { return false }

func (w *apiKeyTestWorkspace) SetProviderAPIKey(_ config.Scope, providerID string, apiKey any) error {
	w.savedProviderID = providerID
	w.savedAPIKey = apiKey.(string)
	return nil
}

func newAPIKeyTestCommon(t *testing.T) (*common.Common, *apiKeyTestWorkspace) {
	t.Helper()
	s := styles.CharmtonePantera()
	ws := &apiKeyTestWorkspace{}
	return &common.Common{Styles: &s, Workspace: ws}, ws
}

// TestAPIKeyInput_WithModelReturnsActionSelectModel is a regression test
// for the pre-existing model-switch/onboarding flow: with a non-nil model,
// submitting a verified key must still produce ActionSelectModel, driven
// through HandleMsg exactly as a real user interaction would (skip past
// the network-calling verifyAPIKey by injecting the "verified" state
// directly, then press the submit key).
func TestAPIKeyInput_WithModelReturnsActionSelectModel(t *testing.T) {
	com, ws := newAPIKeyTestCommon(t)

	provider := catwalk.Provider{ID: catwalk.InferenceProvider("test-provider"), Name: "Test Provider"}
	model := config.SelectedModel{Provider: "test-provider", Model: "test-model"}
	dlg, _ := NewAPIKeyInput(com, false, provider, &model, config.SelectedModelTypeLarge)

	dlg.HandleMsg(ActionChangeAPIKeyState{State: APIKeyInputStateVerified})
	for _, r := range "sk-test-key" {
		dlg.HandleMsg(keyMsg(r))
	}

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	selectAction, ok := action.(ActionSelectModel)
	require.True(t, ok, "expected ActionSelectModel, got %#v", action)
	require.Equal(t, provider, selectAction.Provider)
	require.Equal(t, model, selectAction.Model)
	require.Equal(t, config.SelectedModelTypeLarge, selectAction.ModelType)
	require.Equal(t, "test-provider", ws.savedProviderID)
	require.Equal(t, "sk-test-key", ws.savedAPIKey)
}

// TestAPIKeyInput_WithoutModelReturnsActionProviderConfigured covers the
// new model-less mode (used by the providers-configuration dialog): with a
// nil model, submitting a verified key produces ActionProviderConfigured
// instead of ActionSelectModel.
func TestAPIKeyInput_WithoutModelReturnsActionProviderConfigured(t *testing.T) {
	com, ws := newAPIKeyTestCommon(t)

	provider := catwalk.Provider{ID: catwalk.InferenceProvider("test-provider"), Name: "Test Provider"}
	dlg, _ := NewAPIKeyInput(com, false, provider, nil, "")

	dlg.HandleMsg(ActionChangeAPIKeyState{State: APIKeyInputStateVerified})
	for _, r := range "sk-test-key" {
		dlg.HandleMsg(keyMsg(r))
	}

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	configuredAction, ok := action.(ActionProviderConfigured)
	require.True(t, ok, "expected ActionProviderConfigured, got %#v", action)
	require.Equal(t, "test-provider", configuredAction.ProviderID)
	require.Equal(t, "test-provider", ws.savedProviderID)
	require.Equal(t, "sk-test-key", ws.savedAPIKey)
}
