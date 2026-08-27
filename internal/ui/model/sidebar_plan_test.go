package model

import (
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

func codexModel() *workspace.AgentModel {
	return &workspace.AgentModel{
		CatalogCfg: catwalk.Model{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol"},
		ModelCfg:   config.SelectedModel{Provider: codex.ProviderID, Model: "gpt-5.6-sol"},
	}
}

// TestPlanInfoShowsCodexAllowance: a subscription hides what a per-token
// bill would show, so the sidebar names the plan and how much of it is
// gone.
func TestPlanInfoShowsCodexAllowance(t *testing.T) {
	u := newCursorTestUI(t)
	codex.RecordUsage(codex.Usage{
		Plan: "plus",
		Primary: codex.UsageWindow{
			UsedPercent:   6,
			WindowMinutes: 10080,
			ResetsAt:      time.Now().Add(80 * time.Hour),
		},
	})

	info := u.planInfo(codexModel())
	require.Contains(t, info, "Plus")
	require.Contains(t, info, "6% of weekly limit")
	require.Contains(t, info, "resets in 3d")
}

// TestPlanInfoEmptyForOtherProviders: everyone else already shows a cost,
// and none of them report an allowance to show instead.
func TestPlanInfoEmptyForOtherProviders(t *testing.T) {
	u := newCursorTestUI(t)
	codex.RecordUsage(codex.Usage{Plan: "plus", Primary: codex.UsageWindow{UsedPercent: 6, WindowMinutes: 10080}})

	model := codexModel()
	model.ModelCfg.Provider = "openai"
	require.Empty(t, u.planInfo(model))
	require.Empty(t, u.planInfo(nil))
}

// TestPlanInfoOmitsAccountLabelForSingleAccount pins the byte-identical
// requirement: a provider with only one account (or none cached yet) must
// render exactly as it did before the account-label cache existed.
func TestPlanInfoOmitsAccountLabelForSingleAccount(t *testing.T) {
	u := newCursorTestUI(t)
	codex.RecordUsage(codex.Usage{Plan: "plus", Primary: codex.UsageWindow{UsedPercent: 6, WindowMinutes: 10080}})

	// No cache entry at all yet (e.g. before the startup refresh lands).
	require.Equal(t, "Plus · 6% of weekly limit", u.planInfo(codexModel()))

	// A cache entry that explicitly says "one account" must also omit the
	// label, not just an absent entry.
	u.labels = map[string]accountLabelInfo{codex.ProviderID: {label: "Личный Plus", multiple: false}}
	require.Equal(t, "Plus · 6% of weekly limit", u.planInfo(codexModel()))
}

// TestPlanInfoIncludesAccountLabelForMultipleAccounts covers the actual
// feature: once the provider has more than one account, its label is
// woven into the plan line between the plan and the usage figure.
func TestPlanInfoIncludesAccountLabelForMultipleAccounts(t *testing.T) {
	u := newCursorTestUI(t)
	codex.RecordUsage(codex.Usage{Plan: "plus", Primary: codex.UsageWindow{UsedPercent: 42, WindowMinutes: 10080}})
	u.labels = map[string]accountLabelInfo{codex.ProviderID: {label: "Личный Plus", multiple: true}}

	require.Equal(t, "Plus · Личный Plus · 42% of weekly limit", u.planInfo(codexModel()))
}

// TestSidebarRendersPlanLine checks the line actually reaches the rendered
// sidebar, not just the helper that builds it.
func TestSidebarRendersPlanLine(t *testing.T) {
	u := newCursorTestUI(t)
	codex.RecordUsage(codex.Usage{
		Plan:    "pro",
		Primary: codex.UsageWindow{UsedPercent: 42, WindowMinutes: 10080},
	})

	// modelInfo renders whatever model the workspace cache holds; point it
	// at Codex the same way a signed-in user would.
	u.wsCache.agentCache.value.ready = true
	u.wsCache.agentCache.value.model = *codexModel()

	rendered := u.modelInfo(60)
	require.Contains(t, strings.Join(strings.Fields(rendered), " "), "Pro · 42% of weekly limit")
}
