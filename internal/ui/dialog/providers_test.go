package dialog

import (
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// providersTestWorkspace is a minimal [workspace.Workspace] stub: it must
// embed the full interface (see testWorkspace in internal/ui/model/ui_test.go
// for the rationale) even though these tests only exercise Config().
type providersTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

func (w *providersTestWorkspace) SupportsThreads() bool { return false }

func (w *providersTestWorkspace) Config() *config.Config {
	return w.cfg
}

func newProvidersTestCommon(t *testing.T) *common.Common {
	t.Helper()
	s := styles.BraidDark()
	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}
	cfg.Providers.Set(string(catwalk.InferenceProviderAnthropic), config.ProviderConfig{
		ID: string(catwalk.InferenceProviderAnthropic),
	})
	return &common.Common{
		Styles:    &s,
		Workspace: &providersTestWorkspace{cfg: cfg},
	}
}

func TestNewProviders_ListsCatalogAndCustomEntry(t *testing.T) {
	com := newProvidersTestCommon(t)

	providers, err := NewProviders(com, false)
	require.NoError(t, err)

	knownProviders, err := config.Providers(com.Config())
	require.NoError(t, err)
	require.NotEmpty(t, knownProviders)

	items := providers.list.FilteredItems()
	// One entry per catalog provider, plus the custom-provider entry.
	require.Len(t, items, len(knownProviders)+1)

	var foundCustomEntry, foundConfigured bool
	for _, it := range items {
		item, ok := it.(*ProviderItem)
		require.True(t, ok)
		if item.ID() == customProviderItemID {
			foundCustomEntry = true
			require.False(t, item.configured)
			continue
		}
		if item.ID() == string(catwalk.InferenceProviderAnthropic) {
			foundConfigured = true
			require.True(t, item.configured)
			require.Contains(t, item.Render(60), "Configured")
		}
	}
	require.True(t, foundCustomEntry, "expected a Custom provider… entry")
	require.True(t, foundConfigured, "expected the configured Anthropic provider to be flagged")
}

func TestNewProviders_OnSelectActions(t *testing.T) {
	com := newProvidersTestCommon(t)

	providers, err := NewProviders(com, false)
	require.NoError(t, err)

	action := providers.cfg.onSelect(customProviderItemID)
	require.Equal(t, ActionOpenCustomProviderForm{}, action)

	action = providers.cfg.onSelect(string(catwalk.InferenceProviderAnthropic))
	require.Equal(t, ActionConfigureProvider{ProviderID: string(catwalk.InferenceProviderAnthropic)}, action)
}

func TestProviderItem_RenderShowsConfiguredBadge(t *testing.T) {
	t.Parallel()
	s := styles.BraidDark()
	item := &ProviderItem{
		id:         "my-provider",
		name:       "My Provider",
		configured: true,
		t:          &s,
	}
	rendered := item.Render(40)
	require.True(t, strings.Contains(rendered, "My Provider"))
	require.True(t, strings.Contains(rendered, "Configured"))
}
