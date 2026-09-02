package dialog

import (
	"context"
	"errors"
	"image"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// accountsTestWorkspace is a minimal [workspace.Workspace] stub: it must
// embed the full interface (see providersTestWorkspace's comment) even
// though these tests only exercise Config, ListAccounts, and
// ActivateAccount. activateCalls counts ActivateAccount invocations so a
// test can prove HandleMsg never calls it synchronously.
type accountsTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config

	accs    []accounts.Account
	listErr error

	activateCalls   int
	activateErr     error
	lastActivatedID string
	lastProviderID  string
	lastScope       config.Scope

	refreshCalls            int
	refreshErr              error
	refreshedAccs           []accounts.Account
	lastRefreshedProviderID string
}

func (w *accountsTestWorkspace) SupportsThreads() bool { return false }

// KnownProviders mirrors what the dialog used to compute for itself: the
// embedded catalog for this fake's config.
func (w accountsTestWorkspace) KnownProviders() []catwalk.Provider {
	return providerruntime.Providers(w.cfg.Options.DisableDefaultProviders)
}

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w accountsTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w accountsTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w accountsTestWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (w *accountsTestWorkspace) Config() *config.Config { return w.cfg }

func (w *accountsTestWorkspace) ListAccounts(providerID string) ([]accounts.Account, error) {
	return w.accs, w.listErr
}

func (w *accountsTestWorkspace) ActivateAccount(scope config.Scope, providerID, accountID string) error {
	w.activateCalls++
	w.lastScope = scope
	w.lastProviderID = providerID
	w.lastActivatedID = accountID
	return w.activateErr
}

// AccountCapabilities mirrors the real accounts.CapabilitiesOf the dialog
// used to call directly, so tests that depend on "codex" reporting usage
// (or another provider's RotateOn) still see production behavior.
func (w accountsTestWorkspace) AccountCapabilities(providerID string) workspace.AccountCapabilities {
	c := accounts.CapabilitiesOf(providerID)
	rotateOn := workspace.RotateNever
	switch c.RotateOn {
	case accounts.RotateThreshold:
		rotateOn = workspace.RotateThreshold
	case accounts.RotateRateLimit:
		rotateOn = workspace.RotateRateLimit
	}
	return workspace.AccountCapabilities{Usage: c.Usage, RotateOn: rotateOn}
}

func (w *accountsTestWorkspace) RefreshAccountLimits(_ context.Context, providerID string) ([]accounts.Account, error) {
	w.refreshCalls++
	w.lastRefreshedProviderID = providerID
	if w.refreshErr != nil {
		return nil, w.refreshErr
	}
	return w.refreshedAccs, nil
}

func newAccountsTestCommon(t *testing.T, providerID, activeAccountID string) (*common.Common, *accountsTestWorkspace) {
	t.Helper()
	s := styles.SennitDark()
	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}
	cfg.Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Account: activeAccountID,
	})
	ws := &accountsTestWorkspace{cfg: cfg}
	return &common.Common{Styles: &s, Workspace: ws}, ws
}

// findMsg runs cmd, unwrapping any tea.BatchMsg, and returns the first
// message satisfying match.
func findMsg(t *testing.T, cmd tea.Cmd, match func(tea.Msg) bool) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if match(msg) {
		return msg
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if found := findMsg(t, c, match); found != nil {
				return found
			}
		}
	}
	return nil
}

func isAccountsLoaded(msg tea.Msg) bool {
	_, ok := msg.(ActionAccountsLoaded)
	return ok
}

// loadedAccounts constructs an Accounts dialog, runs its constructor's
// returned cmd to completion, and feeds the ActionAccountsLoaded result
// back into HandleMsg so the dialog reaches accountsStateList.
func loadedAccounts(t *testing.T, com *common.Common, providerID string) *Accounts {
	t.Helper()
	dlg, cmd := NewAccounts(com, providerID)
	loaded := findMsg(t, cmd, isAccountsLoaded)
	require.NotNil(t, loaded, "expected ActionAccountsLoaded from the constructor's cmd")
	action := dlg.HandleMsg(loaded)
	require.Nil(t, action, "loading successfully should not itself produce an Action")
	require.Equal(t, accountsStateList, dlg.state)
	return dlg
}

func TestAccounts_ListsAccountsAndMarksActive(t *testing.T) {
	providerID := "openai"
	com, _ := newAccountsTestCommon(t, providerID, "acct-1")
	com.Workspace.(*accountsTestWorkspace).accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
		{ID: "acct-2", Label: "Personal"},
	}

	dlg := loadedAccounts(t, com, providerID)

	// 2 accounts plus the trailing "Add account…" and "Provider
	// settings…" sentinels; see TestAccounts_AddAccountItemAppendedToList
	// for those entries themselves.
	items := dlg.sd.list.FilteredItems()
	require.Len(t, items, 4)

	var foundActive, foundInactive bool
	for _, it := range items {
		item, ok := it.(*AccountItem)
		if !ok {
			continue
		}
		switch item.ID() {
		case "acct-1":
			foundActive = true
			require.Contains(t, item.Render(60), "Active")
		case "acct-2":
			foundInactive = true
			require.NotContains(t, item.Render(60), "Active")
		}
	}
	require.True(t, foundActive)
	require.True(t, foundInactive)
}

func TestAccounts_UsageShownOnlyForCapableProvider(t *testing.T) {
	usage := accounts.Usage{
		Plan: "plus",
		Primary: accounts.UsageWindow{
			UsedPercent: 42, WindowMinutes: 10080,
		},
	}

	t.Run("codex reports usage", func(t *testing.T) {
		providerID := "codex" // accounts.CapabilitiesOf("codex").Usage == true
		com, _ := newAccountsTestCommon(t, providerID, "")
		com.Workspace.(*accountsTestWorkspace).accs = []accounts.Account{
			{ID: "acct-1", Label: "Work", Usage: usage},
		}
		dlg := loadedAccounts(t, com, providerID)

		// 1 account plus the trailing "Add account…" and "Provider
		// settings…" sentinels.
		items := dlg.sd.list.FilteredItems()
		require.Len(t, items, 3)
		rendered := items[0].(*AccountItem).Render(60)
		require.Contains(t, rendered, "%")
	})

	t.Run("a provider without usage capability shows no usage column", func(t *testing.T) {
		providerID := "openai" // not in accounts' capability registry
		com, _ := newAccountsTestCommon(t, providerID, "")
		com.Workspace.(*accountsTestWorkspace).accs = []accounts.Account{
			// Carries a non-empty Usage anyway; it must not leak into the
			// row for a provider that doesn't report usage.
			{ID: "acct-1", Label: "Work", Usage: usage},
		}
		dlg := loadedAccounts(t, com, providerID)

		// 1 account plus the trailing "Add account…" and "Provider
		// settings…" sentinels.
		items := dlg.sd.list.FilteredItems()
		require.Len(t, items, 3)
		rendered := items[0].(*AccountItem).Render(60)
		require.NotContains(t, rendered, "%")
	})
}

func TestAccounts_LoadErrorEntersErrorState(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "")
	ws.listErr = errors.New("boom")

	dlg, cmd := NewAccounts(com, providerID)
	loaded := findMsg(t, cmd, isAccountsLoaded)
	require.NotNil(t, loaded)

	action := dlg.HandleMsg(loaded)
	require.Equal(t, accountsStateError, dlg.state)
	require.ErrorContains(t, dlg.err, "boom")

	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok)
	require.NotNil(t, cmdAction.Cmd)

	area := image.Rect(0, 0, 80, 24)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.NotPanics(t, func() {
		dlg.Draw(scr, area)
	})
}

// TestAccounts_IgnoresLoadedResultForADifferentProvider is the regression
// test for a stale-provider race: ActionAccountsLoaded is addressed by the
// AccountsID constant, not by dialog instance, so a refresh started for one
// provider that is still in flight when the dialog is closed and reopened
// for a different provider would otherwise land here and render the first
// provider's accounts under the second provider's title.
func TestAccounts_IgnoresLoadedResultForADifferentProvider(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{{ID: "acct-1", Label: "Work"}}
	dlg := loadedAccounts(t, com, providerID)

	action := dlg.HandleMsg(ActionAccountsLoaded{
		ProviderID: "anthropic",
		Accounts:   []accounts.Account{{ID: "other-acct", Label: "Someone else"}},
	})

	require.Nil(t, action, "a mismatched provider result must be dropped, not acted on")
	require.Equal(t, accountsStateList, dlg.state)
	require.Equal(t, providerID, dlg.providerID, "the dialog must stay addressed to its own provider")
}

// TestAccounts_IgnoresActivationResultForADifferentProvider is a
// regression test: accountActivatedMsg was addressed only by the
// AccountsID dialog constant, with no provider check. Activate an account
// in provider A's dialog, esc before the write returns, then open provider
// B's dialog (same AccountsID) — A's stale result used to land in B's
// HandleMsg, closing it out from under the user and issuing a sidebar
// refresh for B when it was A's active account that actually changed.
func TestAccounts_IgnoresActivationResultForADifferentProvider(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{{ID: "acct-1", Label: "Work"}}
	dlg := loadedAccounts(t, com, providerID)

	action := dlg.HandleMsg(accountActivatedMsg{providerID: "anthropic"})

	require.Nil(t, action, "a mismatched provider's activation result must be dropped, not acted on")
	require.Equal(t, accountsStateList, dlg.state, "must not close on another provider's stale result")
}

func TestAccounts_SelectNonActiveAccount_NoIOInHandleMsg(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
		{ID: "acct-2", Label: "Personal"},
	}

	dlg := loadedAccounts(t, com, providerID)

	// Move off the active account (index 0) onto acct-2.
	dlg.sd.list.SetSelected(1)
	require.Equal(t, "acct-2", dlg.sd.selectedID())

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Zero(t, ws.activateCalls, "HandleMsg must not call ActivateAccount synchronously")

	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected ActionCmd carrying the async activation, got %#v", action)

	msg := cmdAction.Cmd()
	require.Equal(t, 1, ws.activateCalls)
	require.Equal(t, "acct-2", ws.lastActivatedID)
	require.Equal(t, providerID, ws.lastProviderID)
	require.Equal(t, config.ScopeGlobal, ws.lastScope)

	closeAction := dlg.HandleMsg(msg)
	require.Equal(t, ActionAccountActivated{ProviderID: providerID}, closeAction)
}

func TestAccounts_SelectActiveAccount_NoOp(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
		{ID: "acct-2", Label: "Personal"},
	}

	dlg := loadedAccounts(t, com, providerID)
	require.Equal(t, "acct-1", dlg.sd.selectedID(), "the active account should start selected")

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.Zero(t, ws.activateCalls)
}

// TestAccounts_EmptyAccountsEntersEmptyState covers a provider whose
// ListAccounts call succeeds but returns nothing to show — a manually
// cleared accounts.json, or a migration that produced no usable
// credential. The dialog must not build a zero-item selectDialog; it
// should show an explicit empty message instead.
func TestAccounts_EmptyAccountsEntersEmptyState(t *testing.T) {
	providerID := "openai"
	com, _ := newAccountsTestCommon(t, providerID, "")

	dlg, cmd := NewAccounts(com, providerID)
	loaded := findMsg(t, cmd, isAccountsLoaded)
	require.NotNil(t, loaded)

	action := dlg.HandleMsg(loaded)
	require.Nil(t, action)
	require.Equal(t, accountsStateEmpty, dlg.state)
	require.Nil(t, dlg.sd, "no selectDialog should be built for an empty account list")

	require.Equal(t, []key.Binding{CloseKey}, dlg.ShortHelp())

	area := image.Rect(0, 0, 80, 24)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.NotPanics(t, func() {
		dlg.Draw(scr, area)
	})
	require.Contains(t, scr.String(), "No accounts stored for this provider.")
}

// TestAccounts_TitleIncludesProviderName covers both ways the dialog can
// resolve a provider's display name: from the configured entry's own Name,
// and falling back to the bare ID when neither the config nor the catalog
// names it.
func TestAccounts_TitleIncludesProviderName(t *testing.T) {
	t.Run("uses the configured provider's name", func(t *testing.T) {
		providerID := "openai"
		com, _ := newAccountsTestCommon(t, providerID, "")
		pc, _ := com.Config().Providers.Get(providerID)
		pc.Name = "My OpenAI"
		com.Config().Providers.Set(providerID, pc)
		com.Workspace.(*accountsTestWorkspace).accs = []accounts.Account{{ID: "acct-1", Label: "Work"}}

		dlg := loadedAccounts(t, com, providerID)
		require.Equal(t, "My OpenAI Accounts", dlg.sd.cfg.title)
	})

	t.Run("falls back to the bare ID when nothing names the provider", func(t *testing.T) {
		providerID := "unknown-provider"
		com, _ := newAccountsTestCommon(t, providerID, "")
		com.Workspace.(*accountsTestWorkspace).accs = []accounts.Account{{ID: "acct-1", Label: "Work"}}

		dlg := loadedAccounts(t, com, providerID)
		require.Equal(t, "unknown-provider Accounts", dlg.sd.cfg.title)
	})
}

func TestAccounts_SelectDisabledAccount_WarnsAndDoesNotActivate(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
		{ID: "acct-2", Label: "Personal", Disabled: true},
	}

	dlg := loadedAccounts(t, com, providerID)
	dlg.sd.list.SetSelected(1)
	require.Equal(t, "acct-2", dlg.sd.selectedID())

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Zero(t, ws.activateCalls)

	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok)
	msg := cmdAction.Cmd()
	infoMsg, ok := msg.(util.InfoMsg)
	require.True(t, ok, "expected a warning util.InfoMsg, got %#v", msg)
	require.Equal(t, util.InfoTypeWarn, infoMsg.Type)
}

// TestAccounts_AddAccountItemAppendedToList covers the sentinel "Add
// account…" and "Provider settings…" entries appended after the real
// accounts, mirroring providers.go's "Custom provider…" entry.
func TestAccounts_AddAccountItemAppendedToList(t *testing.T) {
	providerID := "openai"
	com, _ := newAccountsTestCommon(t, providerID, "acct-1")
	com.Workspace.(*accountsTestWorkspace).accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
		{ID: "acct-2", Label: "Personal"},
	}

	dlg := loadedAccounts(t, com, providerID)

	items := dlg.sd.list.FilteredItems()
	require.Len(t, items, 4)
	addItem, ok := items[len(items)-2].(*AddAccountItem)
	require.True(t, ok, "expected the second-to-last item to be an AddAccountItem, got %#v", items[len(items)-2])
	require.Equal(t, addAccountItemID, addItem.ID())
	settingsItem, ok := items[len(items)-1].(*ProviderSettingsItem)
	require.True(t, ok, "expected the last item to be a ProviderSettingsItem, got %#v", items[len(items)-1])
	require.Equal(t, providerSettingsItemID, settingsItem.ID())
}

// TestAccounts_EditKey_ReturnsActionOpenAccountEdit_NoIO covers ctrl+r: it
// must return ActionOpenAccountEdit for the highlighted account, carrying
// whether it's the active one, without touching the workspace.
func TestAccounts_EditKey_ReturnsActionOpenAccountEdit_NoIO(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
		{ID: "acct-2", Label: "Personal"},
	}

	dlg := loadedAccounts(t, com, providerID)
	dlg.sd.list.SetSelected(1)
	require.Equal(t, "acct-2", dlg.sd.selectedID())

	action := dlg.HandleMsg(tea.KeyPressMsg{Text: "ctrl+r"})
	require.Zero(t, ws.activateCalls)

	edit, ok := action.(ActionOpenAccountEdit)
	require.True(t, ok, "expected ActionOpenAccountEdit, got %#v", action)
	require.Equal(t, providerID, edit.ProviderID)
	require.Equal(t, "acct-2", edit.Account.ID)
	require.False(t, edit.Active)
}

// TestAccounts_EditKey_OnAddAccountItem_NoOp covers pressing ctrl+r while the
// trailing "Add account…" sentinel is highlighted: there is no account to
// edit, so nothing should happen.
func TestAccounts_EditKey_OnAddAccountItem_NoOp(t *testing.T) {
	providerID := "openai"
	com, _ := newAccountsTestCommon(t, providerID, "acct-1")
	com.Workspace.(*accountsTestWorkspace).accs = []accounts.Account{{ID: "acct-1", Label: "Work"}}

	dlg := loadedAccounts(t, com, providerID)
	items := dlg.sd.list.FilteredItems()
	dlg.sd.list.SetSelected(len(items) - 2)
	require.Equal(t, addAccountItemID, dlg.sd.selectedID())

	action := dlg.HandleMsg(tea.KeyPressMsg{Text: "ctrl+r"})
	require.Nil(t, action)
}

// TestAccounts_DeleteKey_ReturnsActionRequestAccountRemoval_NoIO covers
// ctrl+x: it must return ActionRequestAccountRemoval for the highlighted
// account without removing anything itself — removal goes through a
// confirmation dialog the caller opens.
func TestAccounts_DeleteKey_ReturnsActionRequestAccountRemoval_NoIO(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
		{ID: "acct-2", Label: "Personal"},
	}

	dlg := loadedAccounts(t, com, providerID)
	dlg.sd.list.SetSelected(1)

	action := dlg.HandleMsg(tea.KeyPressMsg{Text: "ctrl+x"})
	require.Zero(t, ws.activateCalls)

	remove, ok := action.(ActionRequestAccountRemoval)
	require.True(t, ok, "expected ActionRequestAccountRemoval, got %#v", action)
	require.Equal(t, providerID, remove.ProviderID)
	require.Equal(t, "acct-2", remove.Account.ID)
}

// TestAccounts_SelectAddAccount_ReturnsActionAddAccount_NoIO covers
// selecting "Add account…": it must return ActionAddAccount for the
// dialog's provider synchronously, without activating/switching any
// account (no ActivateAccount call, no tea.Cmd).
func TestAccounts_SelectAddAccount_ReturnsActionAddAccount_NoIO(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
	}

	dlg := loadedAccounts(t, com, providerID)

	items := dlg.sd.list.FilteredItems()
	dlg.sd.list.SetSelected(len(items) - 2)
	require.Equal(t, addAccountItemID, dlg.sd.selectedID())

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Zero(t, ws.activateCalls, "selecting Add account must not activate/switch accounts")

	addAction, ok := action.(ActionAddAccount)
	require.True(t, ok, "expected ActionAddAccount, got %#v", action)
	require.Equal(t, providerID, addAction.ProviderID)
}

// TestAccounts_SelectProviderSettings_ReturnsActionOpenProviderSettings_NoIO
// covers selecting "Provider settings…": it must return
// ActionOpenProviderSettings for the dialog's provider synchronously,
// without touching the workspace.
func TestAccounts_SelectProviderSettings_ReturnsActionOpenProviderSettings_NoIO(t *testing.T) {
	providerID := "openai"
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{
		{ID: "acct-1", Label: "Work"},
	}

	dlg := loadedAccounts(t, com, providerID)

	items := dlg.sd.list.FilteredItems()
	dlg.sd.list.SetSelected(len(items) - 1)
	require.Equal(t, providerSettingsItemID, dlg.sd.selectedID())

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Zero(t, ws.activateCalls, "selecting Provider settings must not activate/switch accounts")

	settingsAction, ok := action.(ActionOpenProviderSettings)
	require.True(t, ok, "expected ActionOpenProviderSettings, got %#v", action)
	require.Equal(t, providerID, settingsAction.ProviderID)
}

// TestAccounts_RefreshLimitsKey_UsageProvider_RefreshesOffThread covers
// ctrl+l for a provider that reports usage: it must not call
// RefreshAccountLimits synchronously, show the loading state while the
// async call is in flight, and rebuild the list from its result through
// the same ActionAccountsLoaded round-trip the initial load uses.
func TestAccounts_RefreshLimitsKey_UsageProvider_RefreshesOffThread(t *testing.T) {
	providerID := "codex" // accounts.CapabilitiesOf("codex").Usage == true
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{{ID: "acct-1", Label: "Work"}}

	dlg := loadedAccounts(t, com, providerID)
	require.True(t, dlg.caps.Usage)

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	require.Zero(t, ws.refreshCalls, "HandleMsg must not call RefreshAccountLimits synchronously")
	require.Equal(t, accountsStateLoading, dlg.state, "the dialog should show progress while refreshing")

	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected ActionCmd carrying the async refresh, got %#v", action)

	ws.refreshedAccs = []accounts.Account{
		{ID: "acct-1", Label: "Work", Usage: accounts.Usage{Plan: "plus", Primary: accounts.UsageWindow{UsedPercent: 7, WindowMinutes: 10080}}},
	}
	loaded := findMsg(t, cmdAction.Cmd, isAccountsLoaded)
	require.NotNil(t, loaded, "expected ActionAccountsLoaded from the refresh cmd")
	require.Equal(t, 1, ws.refreshCalls)
	require.Equal(t, providerID, ws.lastRefreshedProviderID)

	require.Nil(t, dlg.HandleMsg(loaded))
	require.Equal(t, accountsStateList, dlg.state)
	items := dlg.sd.list.FilteredItems()
	require.Contains(t, items[0].(*AccountItem).Render(60), "7%")
}

// TestAccounts_RefreshLimitsKey_HiddenAndInertForNonUsageProvider covers a
// provider that doesn't report usage (accounts.CapabilitiesOf(...).Usage
// false, e.g. a plain api-key provider): "refresh limits" must not appear
// in the help line, and the key must not trigger a refresh.
func TestAccounts_RefreshLimitsKey_HiddenAndInertForNonUsageProvider(t *testing.T) {
	providerID := "openai" // not in accounts' capability registry
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	ws.accs = []accounts.Account{{ID: "acct-1", Label: "Work"}}

	dlg := loadedAccounts(t, com, providerID)
	require.False(t, dlg.caps.Usage)

	for _, b := range dlg.ShortHelp() {
		require.NotContains(t, b.Help().Key, "refresh", "refresh limits must not be offered for a provider with no usage capability")
	}

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	require.Zero(t, ws.refreshCalls, "ctrl+l must not refresh limits for a provider with no usage capability")
	require.Equal(t, accountsStateList, dlg.state)
	_ = action // the filterable list may consume ctrl+l as ordinary filter input; only the no-refresh contract matters here.
}

// TestAccounts_SelectDialogHelpIncludesEditDeleteRefresh is the
// regression test for Edit/Delete/Refresh never reaching the screen: Draw
// delegates entirely to m.sd.Draw in list state (see Accounts.Draw), and
// that method renders its help footer from the wrapped selectDialog's own
// ShortHelp/FullHelp, not from Accounts.ShortHelp/FullHelp — so hints
// defined only on the outer type never appeared in the dialog's help row,
// no matter what Accounts.ShortHelp itself returned. The fix routes them
// through selectDialogConfig.extraHelp so m.sd's own help — the help
// Draw actually renders — includes them.
func TestAccounts_SelectDialogHelpIncludesEditDeleteRefresh(t *testing.T) {
	providerID := "codex" // accounts.CapabilitiesOf("codex").Usage == true
	com, _ := newAccountsTestCommon(t, providerID, "acct-1")
	com.Workspace.(*accountsTestWorkspace).accs = []accounts.Account{{ID: "acct-1", Label: "Work"}}

	dlg := loadedAccounts(t, com, providerID)

	hasKey := func(bindings []key.Binding, k string) bool {
		for _, b := range bindings {
			if b.Help().Key == k {
				return true
			}
		}
		return false
	}

	sdShort := dlg.sd.ShortHelp()
	require.True(t, hasKey(sdShort, "ctrl+r"), "the selectDialog Draw actually renders must include Edit")
	require.True(t, hasKey(sdShort, "ctrl+x"), "the selectDialog Draw actually renders must include Delete")
	require.True(t, hasKey(sdShort, "ctrl+l"), "the selectDialog Draw actually renders must include Refresh for a usage provider")

	found := false
	for _, row := range dlg.sd.FullHelp() {
		if hasKey(row, "ctrl+r") && hasKey(row, "ctrl+x") {
			found = true
		}
	}
	require.True(t, found, "FullHelp must include an Edit/Delete row")
}

// TestAccounts_RefreshLimitsError_KeepsLastLoadedAccounts is the
// regression test for ActionAccountsLoaded assigning m.accs before
// checking msg.Err: a failed refresh (ctrl+l) used to overwrite the
// already-loaded account list with whatever the failed call returned
// (typically nil), even though the dialog only ever renders m.accs
// through m.sd built from the earlier successful load. Losing that data
// on a failure is the wrong invariant regardless of what the current
// error-state view happens to render — a future retry/back-to-list path
// would otherwise inherit an empty list for no reason.
func TestAccounts_RefreshLimitsError_KeepsLastLoadedAccounts(t *testing.T) {
	providerID := "codex" // accounts.CapabilitiesOf("codex").Usage == true
	com, ws := newAccountsTestCommon(t, providerID, "acct-1")
	original := []accounts.Account{{ID: "acct-1", Label: "Work"}}
	ws.accs = original

	dlg := loadedAccounts(t, com, providerID)
	require.Equal(t, original, dlg.accs)

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok)

	ws.refreshErr = errors.New("network down")
	loaded := findMsg(t, cmdAction.Cmd, isAccountsLoaded)
	require.NotNil(t, loaded)

	dlg.HandleMsg(loaded)
	require.Equal(t, accountsStateError, dlg.state)
	require.Equal(t, original, dlg.accs, "a failed refresh must not wipe the last successfully loaded accounts")
}
