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
	sd         *selectDialog // built once accounts are loaded; nil until then
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
	}
	m.spinner = newOAuthSpinner(com.Styles)
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

// accountActivatedMsg carries the outcome of the async ActivateAccount call
// kicked off when the user picks a different account. Like
// ActionAccountsLoaded, it is addressed back to this dialog rather than
// whatever happens to be on top when it lands.
type accountActivatedMsg struct {
	err error
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
		if msg.Err != nil {
			m.state = accountsStateError
			m.err = msg.Err
			return ActionCmd{util.ReportError(msg.Err)}
		}
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
		if msg.err != nil {
			return ActionCmd{util.ReportError(msg.err)}
		}
		return ActionClose{}

	case tea.KeyPressMsg:
		if m.state == accountsStateList {
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
	activeAccountID := ""
	if pc, ok := m.com.Config().Providers.Get(m.providerID); ok {
		activeAccountID = pc.Account
	}
	caps := accounts.CapabilitiesOf(m.providerID)
	t := m.com.Styles
	providerID := m.providerID

	buildItems := func() ([]list.FilterableItem, int, error) {
		items := make([]list.FilterableItem, 0, len(accs))
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
		return items, startIndex, nil
	}

	onSelect := func(id string) Action {
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
	}
}

// providerDisplayName resolves providerID to the name shown in the UI: the
// configured entry's own Name if it has one, else the catalog's name for
// it, else the bare ID as a last resort.
func providerDisplayName(com *common.Common, providerID string) string {
	cfg := com.Config()
	if pc, ok := cfg.Providers.Get(providerID); ok && pc.Name != "" {
		return pc.Name
	}
	for _, p := range config.Providers(cfg) {
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
		return accountActivatedMsg{err: err}
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

// ShortHelp implements [help.KeyMap].
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

// FullHelp implements [help.KeyMap].
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
			parts = append(parts, common.FormatPlanUsage(a.account.Usage.Plan, common.AccountUsageWindows(a.account.Usage)))
		} else {
			parts = append(parts, "limits unknown")
		}
	}
	info := strings.Join(parts, " · ")

	st := defaultListItemStyles(a.t)
	return renderItem(st, title, info, a.Focused(), width, a.Cache(), a.Match())
}
