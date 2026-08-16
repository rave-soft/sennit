package dialog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

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
