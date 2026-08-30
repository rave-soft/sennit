package dialog

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// AccountsID is the identifier for the accounts dialog.
const (
	AccountsID              = "accounts"
	accountsDialogMaxWidth  = 60
	accountsDialogMinHeight = 8
	accountsDialogMaxHeight = 20
)

// addAccountItemID is the sentinel ID for the "Add account…" entry
// appended to the account list, mirroring providers.go's
// customProviderItemID. It can never collide with a real account.Account
// ID, which is always a machine-generated accounts.NextID value.
const addAccountItemID = "__add_account__"

// providerSettingsItemID is the sentinel ID for the "Provider settings…"
// entry appended to the account list, alongside addAccountItemID.
const providerSettingsItemID = "__provider_settings__"

// accountsState tracks the load-then-list lifecycle: ListAccounts is a
// file read, so the dialog opens on a spinner and only becomes the
// [selectDialog] once the accounts are in.
type accountsState int

const (
	accountsStateLoading accountsState = iota
	accountsStateList
	accountsStateError
	accountsStateEmpty
)

// Accounts lets the user switch between a provider's stored credentialed
// accounts (see internal/providers/accounts).
type Accounts struct {
	Base
	com        *common.Common
	providerID string
	state      accountsState
	spinner    spinner.Model
	err        error
	sd         *selectDialog      // built once accounts are loaded; nil until then
	accs       []accounts.Account // last loaded set, kept for e/d's lookup by ID
	caps       accounts.Capabilities
	keyMap     struct{ Edit, Delete, Refresh key.Binding }
}

var _ Dialog = (*Accounts)(nil)

// NewAccounts opens the accounts dialog for providerID and kicks off the
// async ListAccounts call.
func NewAccounts(com *common.Common, providerID string) (*Accounts, tea.Cmd) {
	m := &Accounts{
		Base:       NewBase(com, accountsDialogMaxWidth),
		com:        com,
		providerID: providerID,
		state:      accountsStateLoading,
		caps:       accounts.CapabilitiesOf(providerID),
	}
	m.spinner = newOAuthSpinner(com.Styles)
	// ctrl+x for delete mirrors sessions.go's Delete binding — this list is
	// filterable ("Type to filter"), so a bare letter would be stolen from
	// the filter input, and "d" right next to it is the worst offender: a
	// user typing "default" would trigger a removal dialog instead.
	//
	// Edit can't reuse sessions.go's own ctrl+r->rename pairing verbatim
	// (ctrl+r is free here, but ctrl+e — the more obvious mnemonic — isn't:
	// bubbles' textinput binds it to LineEnd by default, and the filter
	// input inherits that, so ctrl+e would never reach this dialog while
	// the filter has focus). ctrl+r is picked instead, deliberately
	// matching sessions.go's rename key: editing an account's Label is the
	// closest analogue to renaming a session, and reusing the same chord
	// for the same kind of action keeps muscle memory between the two
	// dialogs rather than adding a new arbitrary one.
	m.keyMap.Edit = key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "edit"))
	m.keyMap.Delete = key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "delete"))
	// ctrl+l for "refresh limits" (l for limits): free in both bubbles'
	// textinput.DefaultKeyMap (which this filterable list's filter input
	// inherits — the chords it claims are ctrl+f/b, alt+left/right,
	// ctrl+left/right, alt+f/b/d, alt+backspace/delete, ctrl+w/k/u/h/d,
	// home/end, ctrl+a/e, ctrl+v, tab, up/down, ctrl+n/p — none of them
	// ctrl+l) and in this dialog's own select_dialog.go (enter, ctrl+y,
	// up/down, ctrl+n/p), on top of the ctrl+r/ctrl+x this file already
	// claims above. ctrl+l is also bound globally to "switch model"
	// (model/keys.go), but that binding is unreachable while any dialog
	// is open — handleKeyPressMsg routes every key to the dialog stack
	// first (see internal/ui/model/keypress.go) — which is exactly the
	// precedent ctrl+r/ctrl+y already set here and in select_dialog.go
	// for their own globally-bound chords.
	m.keyMap.Refresh = key.NewBinding(key.WithKeys("ctrl+l"), key.WithHelp("ctrl+l", "refresh limits"))
	return m, tea.Batch(m.spinner.Tick, m.loadAccountsCmd())
}

// loadAccountsCmd reads the provider's accounts off the Update loop. It
// captures com and providerID by value so it doesn't race with the dialog
// being mutated concurrently.
func (m *Accounts) loadAccountsCmd() tea.Cmd {
	com := m.com
	providerID := m.providerID
	return func() tea.Msg {
		accs, err := com.Workspace.ListAccounts(providerID)
		return ActionAccountsLoaded{ProviderID: providerID, Accounts: accs, Err: err}
	}
}

// refreshLimitsCmd asks the workspace to refresh every account's rate-limit
// snapshot off the Update loop, reusing [ActionAccountsLoaded] — the exact
// message loadAccountsCmd's own load delivers — so the resulting list
// rebuilds the same way an initial load or an edit/removal reload does.
// com and providerID are captured by value so this closure doesn't race
// with the dialog being mutated concurrently.
func (m *Accounts) refreshLimitsCmd() tea.Cmd {
	com := m.com
	providerID := m.providerID
	return func() tea.Msg {
		accs, err := com.Workspace.RefreshAccountLimits(com.Context(), providerID)
		return ActionAccountsLoaded{ProviderID: providerID, Accounts: accs, Err: err}
	}
}

// ActionAccountsLoaded carries the result of the async ListAccounts call
// kicked off by NewAccounts. It round-trips back into this same dialog's
// HandleMsg via the DialogAddressed mechanism (see dialog.go), the same
// way oauth.go's oauthSaveDoneMsg/oauthSaveErrMsg do.
type ActionAccountsLoaded struct {
	ProviderID string
	Accounts   []accounts.Account
	Err        error
}

// DialogID implements [DialogAddressed].
func (ActionAccountsLoaded) DialogID() string { return AccountsID }

// ActionAddAccount is sent when "Add account…" is chosen from the accounts
// list, to start a fresh sign-in for ProviderID rather than switching to
// one already on file.
type ActionAddAccount struct {
	ProviderID string
}

// accountActivatedMsg carries the outcome of the async ActivateAccount call
// kicked off when the user picks a different account. Like
// ActionAccountsLoaded, it is addressed back to this dialog rather than
// whatever happens to be on top when it lands.
//
// providerID is checked in HandleMsg for the same reason
// ActionAccountsLoaded.ProviderID is: DialogID routes by the AccountsID
// constant, not by dialog instance. Activate in provider A's dialog, esc
// before the write returns, then open provider B's dialog (same
// AccountsID) — without the check, A's result closed B's dialog out from
// under the user and refreshed B's sidebar label for an account switch
// that happened on A.
type accountActivatedMsg struct {
	providerID string
	err        error
}

// DialogID implements [DialogAddressed].
func (accountActivatedMsg) DialogID() string { return AccountsID }

// ID implements Dialog.
func (m *Accounts) ID() string { return AccountsID }

// HandleMsg implements [Dialog].
func (m *Accounts) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		if m.state == accountsStateLoading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			if cmd != nil {
				return ActionCmd{cmd}
			}
		}
		return nil

	case ActionAccountsLoaded:
		if msg.ProviderID != m.providerID {
			// DialogID routes by the AccountsID constant, not by dialog
			// instance: a refresh started for provider A that is still in
			// flight when the dialog is closed and reopened for provider B
			// (same AccountsID) would otherwise land here and render A's
			// accounts under B's title, with activate/edit/delete then
			// running A's account id against B.
			return nil
		}
		if msg.Err != nil {
			// Keep the last good m.accs on failure. A refresh (ctrl+l)
			// that errors currently switches straight to the error view
			// regardless, but clobbering the already-loaded list here is
			// still the wrong invariant to bake in — it would silently
			// bite any future "retry"/"back to list" flow that expects
			// m.accs to still hold what was on screen before the
			// refresh was kicked off.
			m.state = accountsStateError
			m.err = msg.Err
			return ActionCmd{util.ReportError(msg.Err)}
		}
		m.accs = msg.Accounts
		if len(msg.Accounts) == 0 {
			m.state = accountsStateEmpty
			return nil
		}
		sd, err := newSelectDialog(m.com, m.selectDialogConfig(msg.Accounts))
		if err != nil {
			// buildItems below only ranges over the already-loaded slice
			// above — there is nothing left in it that can fail. Falling
			// back to the error state rather than panicking (unlike
			// sessions.go's genuinely infallible buildItems) is the more
			// defensive call here, since this dialog's data really did
			// come from IO a moment ago.
			m.state = accountsStateError
			m.err = err
			return ActionCmd{util.ReportError(err)}
		}
		m.sd = sd
		m.state = accountsStateList
		return nil

	case accountActivatedMsg:
		if msg.providerID != m.providerID {
			return nil
		}
		if msg.err != nil {
			return ActionCmd{util.ReportError(msg.err)}
		}
		return ActionAccountActivated{ProviderID: m.providerID}

	case tea.KeyPressMsg:
		if m.state == accountsStateList {
			switch {
			case key.Matches(msg, m.keyMap.Edit):
				if a, ok := m.selectedAccount(); ok {
					return ActionOpenAccountEdit{ProviderID: m.providerID, Account: a, Active: a.ID == m.currentActiveAccountID()}
				}
				return nil
			case key.Matches(msg, m.keyMap.Delete):
				if a, ok := m.selectedAccount(); ok {
					return ActionRequestAccountRemoval{ProviderID: m.providerID, Account: a}
				}
				return nil
			case m.caps.Usage && key.Matches(msg, m.keyMap.Refresh):
				m.state = accountsStateLoading
				return ActionCmd{tea.Batch(m.spinner.Tick, m.refreshLimitsCmd())}
			}
			return m.sd.HandleMsg(msg)
		}
		// Nothing to act on while loading, errored, or empty, besides leaving.
		if key.Matches(msg, CloseKey) {
			return ActionClose{}
		}
		return nil
	}
	return nil
}

// selectDialogConfig builds the [selectDialogConfig] for the loaded
// accounts: an item per account, and an onSelect that activates the
// chosen one (refusing a no-op re-select of the active account, or a
// disabled one) via [Accounts.activateAccountCmd].
func (m *Accounts) selectDialogConfig(accs []accounts.Account) selectDialogConfig {
	activeAccountID := m.currentActiveAccountID()
	caps := accounts.CapabilitiesOf(m.providerID)
	t := m.com.Styles
	providerID := m.providerID

	buildItems := func() ([]list.FilterableItem, int, error) {
		items := make([]list.FilterableItem, 0, len(accs)+1)
		startIndex := 0
		for i, a := range accs {
			active := a.ID == activeAccountID
			items = append(items, &AccountItem{
				BaseItem: list.NewBaseItem(),
				account:  a,
				active:   active,
				caps:     caps,
				t:        t,
			})
			if active {
				startIndex = i
			}
		}
		items = append(items, &AddAccountItem{BaseItem: list.NewBaseItem(), t: t})
		items = append(items, &ProviderSettingsItem{BaseItem: list.NewBaseItem(), t: t})
		return items, startIndex, nil
	}

	onSelect := func(id string) Action {
		if id == addAccountItemID {
			return ActionAddAccount{ProviderID: providerID}
		}
		if id == providerSettingsItemID {
			return ActionOpenProviderSettings{ProviderID: providerID}
		}
		if id == activeAccountID {
			return nil
		}
		for _, a := range accs {
			if a.ID != id {
				continue
			}
			if a.Disabled {
				return ActionCmd{util.ReportWarn("This account is disabled")}
			}
			break
		}
		return ActionCmd{m.activateAccountCmd(providerID, id)}
	}

	return selectDialogConfig{
		id:            AccountsID,
		title:         providerDisplayName(m.com, m.providerID) + " Accounts",
		maxWidth:      accountsDialogMaxWidth,
		dynamicHeight: true,
		minHeight:     accountsDialogMinHeight,
		maxHeight:     accountsDialogMaxHeight,
		buildItems:    buildItems,
		onSelect:      onSelect,
		// Draw delegates entirely to the built selectDialog (see
		// Accounts.Draw), so Edit/Delete/Refresh must reach the screen
		// through the selectDialog's own help footer via extraHelp —
		// otherwise these hints are defined on Accounts.ShortHelp/FullHelp
		// but never actually rendered.
		extraHelp: func() []key.Binding {
			bindings := []key.Binding{m.keyMap.Edit, m.keyMap.Delete}
			if m.caps.Usage {
				bindings = append(bindings, m.keyMap.Refresh)
			}
			return bindings
		},
	}
}

// currentActiveAccountID reads the provider's active account ID off the
// already-loaded config — a cheap in-memory read, not IO, so it's fine to
// call from HandleMsg directly (see internal/ui/AGENTS.md's dialog rules).
func (m *Accounts) currentActiveAccountID() string {
	if pc, ok := m.com.Config().Providers.Get(m.providerID); ok {
		return pc.Account
	}
	return ""
}

// selectedAccount returns the account currently highlighted in the list,
// looked up in the last loaded set by ID. It reports false for the
// "Add account…" entry or when nothing is loaded yet.
func (m *Accounts) selectedAccount() (accounts.Account, bool) {
	if m.sd == nil {
		return accounts.Account{}, false
	}
	id := m.sd.selectedID()
	if id == "" || id == addAccountItemID {
		return accounts.Account{}, false
	}
	for _, a := range m.accs {
		if a.ID == id {
			return a, true
		}
	}
	return accounts.Account{}, false
}

// providerDisplayName resolves providerID to the name shown in the UI: the
// configured entry's own Name if it has one, else the catalog's name for
// it, else the bare ID as a last resort.
func providerDisplayName(com *common.Common, providerID string) string {
	cfg := com.Config()
	if pc, ok := cfg.Providers.Get(providerID); ok && pc.Name != "" {
		return pc.Name
	}
	for _, p := range com.Workspace.KnownProviders() {
		if string(p.ID) == providerID {
			return p.Name
		}
	}
	return providerID
}

// activateAccountCmd persists the switch off the Update loop, mirroring
// sessions.go's deleteSessionCmd: the workspace is captured by value so
// this closure doesn't race with the dialog being mutated concurrently.
func (m *Accounts) activateAccountCmd(providerID, id string) tea.Cmd {
	ws := m.com.Workspace
	return func() tea.Msg {
		err := ws.ActivateAccount(config.ScopeGlobal, providerID, id)
		return accountActivatedMsg{providerID: providerID, err: err}
	}
}

// Draw implements [Dialog].
func (m *Accounts) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	if m.state == accountsStateList {
		return m.sd.Draw(scr, area)
	}
	m.Resize(area)
	view := m.Frame(m.innerContent())
	DrawCenterCursor(scr, area, view, nil)
	return nil
}

func (m *Accounts) innerContent() string {
	t := m.com.Styles
	innerWidth := m.InnerWidth()

	if m.state == accountsStateError {
		msg := "Failed to load accounts."
		if m.err != nil {
			msg = m.err.Error()
		}
		return t.Dialog.OAuth.ErrorText.
			Width(innerWidth).
			Padding(1).
			Render(msg)
	}

	if m.state == accountsStateEmpty {
		return t.Dialog.OAuth.StatusText.
			Width(innerWidth).
			Align(lipgloss.Center).
			Padding(1).
			Render("No accounts stored for this provider.\nSign in again to add one.")
	}

	return lipgloss.NewStyle().
		Width(innerWidth).
		Align(lipgloss.Center).
		Render(
			t.Dialog.OAuth.Success.Render(m.spinner.View()) +
				t.Dialog.OAuth.StatusText.Render(" Loading accounts..."),
		)
}

// ShortHelp implements [help.KeyMap]. In list state this defers entirely to
// the wrapped selectDialog, which already folds in Edit/Delete/Refresh via
// its extraHelp hook (see selectDialogConfig above) — the same bindings
// Draw's help footer actually renders.
func (m *Accounts) ShortHelp() []key.Binding {
	switch m.state {
	case accountsStateList:
		return m.sd.ShortHelp()
	case accountsStateError, accountsStateEmpty:
		return []key.Binding{CloseKey}
	default:
		// Nothing actionable while loading.
		return nil
	}
}

// FullHelp implements [help.KeyMap]. See ShortHelp's comment.
func (m *Accounts) FullHelp() [][]key.Binding {
	if m.state == accountsStateList {
		return m.sd.FullHelp()
	}
	return [][]key.Binding{m.ShortHelp()}
}

var _ help.KeyMap = (*Accounts)(nil)

// AccountItem represents an account list item.
type AccountItem struct {
	list.BaseItem
	account accounts.Account
	active  bool
	caps    accounts.Capabilities
	t       *styles.Styles
}

var _ ListItem = (*AccountItem)(nil)

// Filter implements ListItem.
func (a *AccountItem) Filter() string {
	if a.account.Label != "" {
		return a.account.Label
	}
	return a.account.ID
}

// ID implements ListItem.
func (a *AccountItem) ID() string {
	return a.account.ID
}

// Render implements ListItem.
func (a *AccountItem) Render(width int) string {
	title := a.account.Label
	if title == "" {
		title = a.account.ID
	}

	var parts []string
	if a.active {
		parts = append(parts, "Active")
	}
	if a.account.Disabled {
		parts = append(parts, "Disabled")
	}
	if a.caps.Usage {
		if a.account.Usage.Known() {
			parts = append(parts, common.FormatPlanUsage(a.account.Usage.Plan, common.AccountUsageWindows(a.account.Usage), ""))
		} else {
			parts = append(parts, "limits unknown")
		}
	}
	info := strings.Join(parts, " · ")

	st := defaultListItemStyles(a.t)
	return renderItem(st, title, info, a.Focused(), width, a.Cache(), a.Match())
}

// AddAccountItem is the "Add account…" entry appended to the account list,
// mirroring ProviderItem's "Custom provider…" entry in providers.go.
// Selecting it starts a fresh sign-in for the dialog's provider rather than
// switching to an existing account.
type AddAccountItem struct {
	list.BaseItem
	t *styles.Styles
}

var _ ListItem = (*AddAccountItem)(nil)

// Filter implements ListItem.
func (a *AddAccountItem) Filter() string {
	return "Add account…"
}

// ID implements ListItem.
func (a *AddAccountItem) ID() string {
	return addAccountItemID
}

// Render implements ListItem.
func (a *AddAccountItem) Render(width int) string {
	st := defaultListItemStyles(a.t)
	return renderItem(st, "Add account…", "", a.Focused(), width, a.Cache(), a.Match())
}

// ProviderSettingsItem is the "Provider settings…" entry appended to the
// account list, after AddAccountItem. Selecting it opens
// [ProviderSettings] for the dialog's provider (proxy, and — where the
// provider supports it — automatic account rotation).
type ProviderSettingsItem struct {
	list.BaseItem
	t *styles.Styles
}

var _ ListItem = (*ProviderSettingsItem)(nil)

// Filter implements ListItem.
func (p *ProviderSettingsItem) Filter() string {
	return "Provider settings…"
}

// ID implements ListItem.
func (p *ProviderSettingsItem) ID() string {
	return providerSettingsItemID
}

// Render implements ListItem.
func (p *ProviderSettingsItem) Render(width int) string {
	st := defaultListItemStyles(p.t)
	return renderItem(st, "Provider settings…", "", p.Focused(), width, p.Cache(), p.Match())
}
