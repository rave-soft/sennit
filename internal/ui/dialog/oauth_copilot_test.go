package dialog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// copilotProxyTestWorkspace is a minimal [workspace.Workspace] stub —
// mirroring accountsTestWorkspace's comment on why it must embed the full
// interface — exposing only the config NewOAuthCopilot reads for the
// configured proxy.
type copilotProxyTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

// KnownProviders mirrors what the UI used to compute for itself:
// the embedded catalog for this fake's config.
func (w copilotProxyTestWorkspace) KnownProviders() []catwalk.Provider {
	return config.Providers(w.cfg)
}

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w copilotProxyTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w copilotProxyTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w copilotProxyTestWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (w *copilotProxyTestWorkspace) Config() *config.Config { return w.cfg }

// TestOAuthCopilotUsesConfiguredProxy pins B10: a proxy already set for the
// copilot provider must be what the device flow uses, without the dialog
// growing a proxy-entry step of its own (that would change every Copilot
// sign-in's UX, not just the ones that need a proxy — see
// TestOAuthCopilotSkipsProxyStep).
func TestOAuthCopilotUsesConfiguredProxy(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}
	cfg.Providers.Set("copilot", config.ProviderConfig{ID: "copilot", ProxyURL: "socks5://127.0.0.1:1080"})
	com := &common.Common{Styles: &s, Workspace: &copilotProxyTestWorkspace{cfg: cfg}}

	provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
	dlg, _ := NewOAuthCopilot(com, false, provider, nil, false)

	oc, ok := dlg.oAuthProvider.(*OAuthCopilot)
	require.True(t, ok)
	require.Equal(t, "socks5://127.0.0.1:1080", oc.proxy)
}

// TestOAuthCopilotNoProxyConfigured pins the no-proxy case unchanged: a
// user with nothing configured gets an empty proxy, exactly like before any
// of this proxy support existed.
func TestOAuthCopilotNoProxyConfigured(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	com := &common.Common{Styles: &s}

	provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
	dlg, _ := NewOAuthCopilot(com, false, provider, nil, false)

	oc, ok := dlg.oAuthProvider.(*OAuthCopilot)
	require.True(t, ok)
	require.Empty(t, oc.proxy)
}
