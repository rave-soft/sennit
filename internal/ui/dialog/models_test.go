package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// modelsTestWorkspace is a minimal [workspace.Workspace] stub, following the
// pattern in providers_test.go: embed the full interface so unimplemented
// methods panic only if a test path actually calls them.
type modelsTestWorkspace struct {
	workspace.Workspace
	cfg               *config.Config
	setConfigFieldErr error
	setConfigFields   map[string]any
}

func (w *modelsTestWorkspace) SupportsThreads() bool { return false }

func (w *modelsTestWorkspace) Config() *config.Config {
	return w.cfg
}

func (w *modelsTestWorkspace) SetConfigField(scope config.Scope, field string, value any) error {
	if w.setConfigFields == nil {
		w.setConfigFields = map[string]any{}
	}
	w.setConfigFields[field] = value
	return w.setConfigFieldErr
}

func newModelsTestCommon(t *testing.T, cfg *config.Config) *common.Common {
	t.Helper()
	s := styles.CharmtonePantera()
	return &common.Common{
		Styles:    &s,
		Workspace: &modelsTestWorkspace{cfg: cfg},
	}
}

func newModelsTestConfig() *config.Config {
	return &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}
}

// flattenModelItems collects every *ModelItem across every group, mirroring
// what the list actually renders (see ModelsList.SetGroups).
func flattenModelItems(groups []ModelGroup) []*ModelItem {
	var items []*ModelItem
	for _, g := range groups {
		items = append(items, g.Items...)
	}
	return items
}

func TestSetProviderItems_OnlyConfiguredProvidersAppear(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.Providers.Set(string(catwalk.InferenceProviderAnthropic), config.ProviderConfig{
		ID: string(catwalk.InferenceProviderAnthropic),
	})
	com := newModelsTestCommon(t, cfg)

	m, err := NewModels(com)
	require.NoError(t, err)

	var nonRecentGroups []ModelGroup
	for _, g := range m.list.groups {
		if g.Title != "Recently used" {
			nonRecentGroups = append(nonRecentGroups, g)
		}
	}
	require.Len(t, nonRecentGroups, 1, "only the configured Anthropic provider should get a group")
	require.Equal(t, "Anthropic", nonRecentGroups[0].Title)
	require.NotEmpty(t, nonRecentGroups[0].Items, "the Anthropic group should carry its catalog models")

	for _, item := range flattenModelItems(m.list.groups) {
		require.NotEqual(t, string(catwalk.InferenceProviderOpenAI), string(item.prov.ID),
			"openai is not configured and must not appear anywhere in the dialog")
	}
}

func TestSetProviderItems_DisabledCatalogProviderIsSkipped(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.Providers.Set(string(catwalk.InferenceProviderAnthropic), config.ProviderConfig{
		ID:      string(catwalk.InferenceProviderAnthropic),
		Disable: true,
	})
	com := newModelsTestCommon(t, cfg)

	m, err := NewModels(com)
	require.NoError(t, err)

	require.Empty(t, flattenModelItems(m.list.groups),
		"a disabled provider must not contribute any model items")
}

func TestSetProviderItems_RecentEntriesFromUnconfiguredProviderAreDropped(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.Providers.Set(string(catwalk.InferenceProviderAnthropic), config.ProviderConfig{
		ID: string(catwalk.InferenceProviderAnthropic),
	})

	// A recent OpenAI model, but OpenAI is not (or no longer) configured —
	// itemsMap only ever contains configured providers' models now, so this
	// entry should be silently dropped from "Recently used" rather than
	// crashing or leaking an unconfigured provider into the dialog.
	cfg.RecentModels = []config.SelectedModel{
		{Provider: string(catwalk.InferenceProviderOpenAI), Model: "gpt-4o"},
	}
	com := newModelsTestCommon(t, cfg)

	m, err := NewModels(com)
	require.NoError(t, err)

	for _, g := range m.list.groups {
		require.NotEqual(t, "Recently used", g.Title,
			"the only recent entry belonged to an unconfigured provider, so no recent group should be added")
	}

	// Pruning the stale recent entry writes back through SetConfigField;
	// confirm that happened rather than silently mutating cfg in place.
	ws := com.Workspace.(*modelsTestWorkspace)
	require.Contains(t, ws.setConfigFields, "recent_models")
	require.Empty(t, ws.setConfigFields["recent_models"])
}

func TestSetProviderItems_EmptyStateShowsPlaceholderAndNoPanic(t *testing.T) {
	cfg := newModelsTestConfig()
	com := newModelsTestCommon(t, cfg)

	m, err := NewModels(com)
	require.NoError(t, err)

	require.Empty(t, flattenModelItems(m.list.groups),
		"no configured providers means no selectable model items")
	require.Len(t, m.list.groups, 1, "a single placeholder group should be shown")
	require.Contains(t, m.list.groups[0].Title, "Configure Providers")

	// Navigation must not panic when there is nothing selectable.
	require.Nil(t, m.list.SelectedItem())
	require.NotPanics(t, func() {
		m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
		m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
		m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	})
}

func TestModelGroup_RenderNoLongerShowsConfiguredBadge(t *testing.T) {
	t.Parallel()
	s := styles.CharmtonePantera()
	g := NewModelGroup(&s, "Anthropic")
	rendered := g.Render(60)
	require.NotContains(t, rendered, "Configured")
}

// TestModels_NoTabToggle covers that there is no key binding to switch
// between model types (there is only one model now), so pressing tab must
// not disturb the list.
func TestModels_NoTabToggle(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.Providers.Set(string(catwalk.InferenceProviderAnthropic), config.ProviderConfig{
		ID: string(catwalk.InferenceProviderAnthropic),
	})
	com := newModelsTestCommon(t, cfg)

	m, err := NewModels(com)
	require.NoError(t, err)

	for _, kb := range m.ShortHelp() {
		require.NotEqual(t, "toggle type", kb.Help().Desc,
			"there is no task-type toggle anymore")
	}

	groupsBefore := m.list.groups
	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.IsType(t, ActionCmd{}, action,
		"tab is no longer a dedicated toggle binding — it now just falls through to the filter input like any other key")
	require.Equal(t, groupsBefore, m.list.groups, "tab must not rebuild the list around a different model type")
}

// TestModels_SelectAlwaysTargetsLargeSlot covers that choosing a model from
// this dialog always reports [config.SelectedModelTypeLarge], the only
// model type the dialog operates on.
func TestModels_SelectAlwaysTargetsLargeSlot(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.Providers.Set(string(catwalk.InferenceProviderAnthropic), config.ProviderConfig{
		ID: string(catwalk.InferenceProviderAnthropic),
	})
	com := newModelsTestCommon(t, cfg)

	m, err := NewModels(com)
	require.NoError(t, err)

	items := flattenModelItems(m.list.groups)
	require.NotEmpty(t, items, "the configured Anthropic provider should carry catalog models")
	m.list.SetSelectedItem(items[0].ID())

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	selectAction, ok := action.(ActionSelectModel)
	require.True(t, ok, "expected ActionSelectModel, got %T", action)
	require.Equal(t, config.SelectedModelTypeLarge, selectAction.ModelType)
}
