package dialog

import (
	"errors"
	"image"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/providers/accounts"
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
}

func (w *accountsTestWorkspace) SupportsThreads() bool { return false }

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
	require.Equal(t, ActionClose{}, closeAction)
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
