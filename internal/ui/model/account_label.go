package model

import (
	"cmp"
	"maps"

	tea "charm.land/bubbletea/v2"
)

// accountLabelsState caches, per provider, the display label of that
// provider's currently active account — the one extra piece of context
// the sidebar's plan line shows once a provider has more than one account
// on file (see sidebar.go's planInfo and common.FormatPlanUsage).
//
// This exists because ListAccounts is a file read (internal/ui/AGENTS.md
// forbids IO on the render path), so the label can't be computed inside
// planInfo itself. It is refreshed only at the points where the answer
// can actually change: at startup (UI.Init) and after the accounts dialog
// activates, adds, edits, or removes an account (dialog_actions.go,
// update_settings.go). labels is replaced wholesale on every refresh, the
// same way integrationsState's mcpStates/skillStates are — see its
// mcpVersion doc comment for why sidebarSig keys off a version counter
// instead of the map's contents.
type accountLabelsState struct {
	labels        map[string]accountLabelInfo
	labelsVersion int
}

// accountLabelInfo is what the sidebar needs about one provider's active
// account: its display label, and whether the provider has more than one
// account at all. label is only ever rendered when multiple is true — a
// single-account provider's sidebar must stay exactly as it is today.
type accountLabelInfo struct {
	label    string
	multiple bool
}

// accountLabelsLoadedMsg carries the result of refreshAccountLabelCmd.
type accountLabelsLoadedMsg struct {
	providerID string
	info       accountLabelInfo
}

// refreshAccountLabelCmd re-reads providerID's accounts off the Update
// loop and reports its active account's label, so the sidebar can render
// it without doing IO itself. providerID == "" is a no-op (nothing to
// refresh, e.g. no model configured yet); a lookup failure just clears
// the cached entry rather than surfacing an error dialog — the sidebar
// silently falls back to its no-label form, the same as for a provider
// with only one account.
func (m *UI) refreshAccountLabelCmd(providerID string) tea.Cmd {
	if providerID == "" {
		return nil
	}
	com := m.com
	return func() tea.Msg {
		accs, err := com.Workspace.ListAccounts(providerID)
		if err != nil || len(accs) <= 1 {
			return accountLabelsLoadedMsg{providerID: providerID}
		}
		activeID := ""
		if pc, ok := com.Config().Providers.Get(providerID); ok {
			activeID = pc.Account
		}
		label := ""
		for _, a := range accs {
			if a.ID == activeID {
				label = cmp.Or(a.Label, a.ID)
				break
			}
		}
		return accountLabelsLoadedMsg{providerID: providerID, info: accountLabelInfo{label: label, multiple: true}}
	}
}

// applyAccountLabelsLoaded stores the refreshed label and bumps
// labelsVersion so sidebarSig (sidebar.go) notices the cache changed.
func (m *UI) applyAccountLabelsLoaded(msg accountLabelsLoadedMsg) {
	labels := make(map[string]accountLabelInfo, len(m.labels)+1)
	maps.Copy(labels, m.labels)
	labels[msg.providerID] = msg.info
	m.labels = labels
	m.labelsVersion++
}

// accountLabelFor returns the cached display label for providerID's
// active account, or "" when the provider has one account or fewer, or
// the cache hasn't been populated for it yet.
func (m *UI) accountLabelFor(providerID string) string {
	info, ok := m.labels[providerID]
	if !ok || !info.multiple {
		return ""
	}
	return info.label
}
