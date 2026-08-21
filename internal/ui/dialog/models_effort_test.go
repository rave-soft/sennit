package dialog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// effortModelItem is a picker entry for a reasoning model offering
// low/high/xhigh and defaulting to low.
func effortModelItem(t *testing.T) *ModelItem {
	t.Helper()
	sty := styles.SennitDark()
	return NewModelItem(&sty,
		catwalk.Provider{ID: catwalk.InferenceProvider("codex")},
		catwalk.Model{
			ID:                     "gpt-5.6-sol",
			Name:                   "GPT-5.6-Sol",
			CanReason:              true,
			ReasoningLevels:        []string{"low", "high", "xhigh"},
			DefaultReasoningEffort: "low",
		},
		false)
}

// TestRememberEffortRestoresTheModelsLastLevel: the picker builds its
// SelectedModel out of the catalog entry, so before this every trip through
// the model dialog - including re-picking the model already active - reset
// the effort to the catalog default and said nothing about it.
func TestRememberEffortRestoresTheModelsLastLevel(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.RecentModels = []config.SelectedModel{
		{Provider: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "xhigh"},
	}
	m, _, err := NewModels(newModelsTestCommon(t, cfg))
	require.NoError(t, err)

	require.Equal(t, "xhigh", m.rememberEffort(effortModelItem(t)).ReasoningEffort)
}

// TestRememberEffortPrefersTheLiveSelection: the current selection is
// written before the recent list is rebuilt from it, so it is the fresher
// of the two whenever they disagree.
func TestRememberEffortPrefersTheLiveSelection(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.Model = config.SelectedModel{Provider: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"}
	cfg.RecentModels = []config.SelectedModel{
		{Provider: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "xhigh"},
	}
	m, _, err := NewModels(newModelsTestCommon(t, cfg))
	require.NoError(t, err)

	require.Equal(t, "high", m.rememberEffort(effortModelItem(t)).ReasoningEffort)
}

// TestRememberEffortDropsALevelTheModelNoLongerOffers: a catalog entry can
// be re-fetched with a different ladder, and sending a level the provider
// has never heard of is worse than falling back to the default.
func TestRememberEffortDropsALevelTheModelNoLongerOffers(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.RecentModels = []config.SelectedModel{
		{Provider: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "ultra"},
	}
	m, _, err := NewModels(newModelsTestCommon(t, cfg))
	require.NoError(t, err)

	require.Equal(t, "low", m.rememberEffort(effortModelItem(t)).ReasoningEffort,
		"the catalog default stands in for a level that is gone")
}

// TestRememberEffortLeavesAnUnknownModelAlone: a model picked for the first
// time has nothing to remember and must keep its catalog default.
func TestRememberEffortLeavesAnUnknownModelAlone(t *testing.T) {
	cfg := newModelsTestConfig()
	cfg.RecentModels = []config.SelectedModel{
		{Provider: "codex", Model: "gpt-5.6-terra", ReasoningEffort: "xhigh"},
	}
	m, _, err := NewModels(newModelsTestCommon(t, cfg))
	require.NoError(t, err)

	require.Equal(t, "low", m.rememberEffort(effortModelItem(t)).ReasoningEffort)
}
