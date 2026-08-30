package model

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// accountLabelTestWorkspace is a minimal workspace.Workspace stub for
// exercising refreshAccountLabelCmd's own logic (as opposed to
// planInfo's rendering, covered by sidebar_plan_test.go against a
// hand-set cache).
type accountLabelTestWorkspace struct {
	workspace.Workspace
	cfg  *config.Config
	accs []accounts.Account
	err  error
}

// KnownProviders mirrors what the UI used to compute for itself:
// the embedded catalog for this fake's config.
func (w accountLabelTestWorkspace) KnownProviders() []catwalk.Provider {
	return config.Providers(w.cfg)
}

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w accountLabelTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w accountLabelTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w accountLabelTestWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (w *accountLabelTestWorkspace) Config() *config.Config { return w.cfg }

func (w *accountLabelTestWorkspace) ListAccounts(string) ([]accounts.Account, error) {
	return w.accs, w.err
}

func newAccountLabelTestUI(t *testing.T, activeAccountID string, accs []accounts.Account) (*UI, *accountLabelTestWorkspace) {
	t.Helper()
	const providerID = "codex"
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set(providerID, config.ProviderConfig{ID: providerID, Account: activeAccountID})
	ws := &accountLabelTestWorkspace{
		cfg:  &config.Config{Providers: providers, Options: &config.Options{}},
		accs: accs,
	}
	com := common.DefaultCommon(context.Background(), ws)
	return &UI{com: com}, ws
}

// TestRefreshAccountLabelCmd_MultipleAccounts_ReportsActiveLabel is the
// heart of the "shown when there is more than one account" rule.
func TestRefreshAccountLabelCmd_MultipleAccounts_ReportsActiveLabel(t *testing.T) {
	u, _ := newAccountLabelTestUI(t, "acct-2", []accounts.Account{
		{ID: "acct-1", Label: "Work"},
		{ID: "acct-2", Label: "Личный Plus"},
	})

	cmd := refreshAccountLabelCmd(u.com, "codex")
	require.NotNil(t, cmd)
	msg, ok := cmd().(accountLabelsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, "codex", msg.providerID)
	require.True(t, msg.info.multiple)
	require.Equal(t, "Личный Plus", msg.info.label)
}

// TestRefreshAccountLabelCmd_SingleAccount_ReportsNoLabel is the "hidden
// when there is only one account" rule at the cmd layer (sidebar_plan_test
// covers it at the render layer).
func TestRefreshAccountLabelCmd_SingleAccount_ReportsNoLabel(t *testing.T) {
	u, _ := newAccountLabelTestUI(t, "acct-1", []accounts.Account{
		{ID: "acct-1", Label: "Only Account"},
	})

	msg, ok := refreshAccountLabelCmd(u.com, "codex")().(accountLabelsLoadedMsg)
	require.True(t, ok)
	require.False(t, msg.info.multiple)
	require.Empty(t, msg.info.label)
}

// TestRefreshAccountLabelCmd_EmptyProviderID_NoCmd covers the startup
// no-model case: there is nothing to refresh yet, so no IO should be
// scheduled at all.
func TestRefreshAccountLabelCmd_EmptyProviderID_NoCmd(t *testing.T) {
	u, _ := newAccountLabelTestUI(t, "acct-1", nil)
	require.Nil(t, refreshAccountLabelCmd(u.com, ""))
}

// TestRefreshAccountLabelCmd_ListErrorClearsCache mirrors the config-level
// contract of leaving no stale label behind when the read itself fails.
func TestRefreshAccountLabelCmd_ListErrorClearsCache(t *testing.T) {
	u, ws := newAccountLabelTestUI(t, "acct-1", []accounts.Account{
		{ID: "acct-1"}, {ID: "acct-2"},
	})
	ws.err = errors.New("boom")

	msg, ok := refreshAccountLabelCmd(u.com, "codex")().(accountLabelsLoadedMsg)
	require.True(t, ok)
	require.False(t, msg.info.multiple)
	require.Empty(t, msg.info.label)
}

// TestApplyAccountLabelsLoaded_BumpsVersionAndStores proves the cache
// write path: the sidebar's cache-invalidation signature (sidebarSig)
// keys off labelsVersion instead of diffing the map (see mcpVersion's
// doc comment in update_integrations.go), so a real store must bump it.
func TestApplyAccountLabelsLoaded_BumpsVersionAndStores(t *testing.T) {
	u := &UI{}
	require.Zero(t, u.labelsVersion)

	u.applyAccountLabelsLoaded(accountLabelsLoadedMsg{
		providerID: "codex",
		info:       accountLabelInfo{label: "Личный Plus", multiple: true},
	})
	require.Equal(t, 1, u.labelsVersion)
	require.Equal(t, "Личный Plus", u.accountLabelFor("codex"))

	// A second provider's refresh must not clobber the first's entry.
	u.applyAccountLabelsLoaded(accountLabelsLoadedMsg{
		providerID: "openai",
		info:       accountLabelInfo{},
	})
	require.Equal(t, 2, u.labelsVersion)
	require.Equal(t, "Личный Plus", u.accountLabelFor("codex"), "an unrelated provider's refresh must not touch this one's cached label")
	require.Empty(t, u.accountLabelFor("openai"))
}
