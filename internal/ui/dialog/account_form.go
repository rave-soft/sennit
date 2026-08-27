package dialog

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/proxyhttp"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// AccountFormID is the identifier for the account edit form dialog.
const AccountFormID = "account_form"

const accountFormMaxWidth = 60

// accountFormField indexes the focusable fields, in tab order.
type accountFormField int

const (
	accountFormFieldLabel accountFormField = iota
	accountFormFieldProxy
	accountFormFieldEnabled
	accountFormFieldCount
)

// AccountForm edits the user-editable fields of an existing stored account
// (see internal/providers/accounts.Account): Label, ProxyURL, and Disabled.
// It does no IO itself — submitting returns [ActionSubmitAccountForm] and
// the caller (ui.go) does the async UpdateAccount call in a tea.Cmd. The
// result rounds back as [ActionAccountFormResult] (see actions.go's
// [ActionCustomProviderResult] doc comment for how this round-trip works).
type AccountForm struct {
	Base
	com *common.Common

	providerID string
	account    accounts.Account
	// active is whether this account is the provider's current one. An
	// active account cannot be disabled from here — see submit().
	active bool

	label   textinput.Model
	proxy   textinput.Model
	enabled bool

	focus      accountFormField
	submitting bool
	errMsg     string

	fieldRow map[accountFormField]int

	help help.Model

	keyMap struct {
		Next   key.Binding
		Prev   key.Binding
		Toggle key.Binding
		Submit key.Binding
		Close  key.Binding
	}
}

var _ Dialog = (*AccountForm)(nil)

// NewAccountForm creates the edit form for account, one of providerID's
// stored accounts. active tells the form whether account is the provider's
// currently active one, which governs whether Enabled can be turned off.
func NewAccountForm(com *common.Common, providerID string, account accounts.Account, active bool) *AccountForm {
	m := &AccountForm{
		Base:       NewBase(com, accountFormMaxWidth),
		com:        com,
		providerID: providerID,
		account:    account,
		active:     active,
		enabled:    !account.Disabled,
	}

	m.label = textinput.New()
	m.label.SetVirtualCursor(false)
	m.label.Placeholder = "Shown as the account ID when empty"
	m.label.SetStyles(com.Styles.TextInput)
	m.label.Prompt = "> "
	m.label.SetValue(account.Label)
	m.label.Focus()

	m.proxy = textinput.New()
	m.proxy.SetVirtualCursor(false)
	m.proxy.Placeholder = "Empty inherits the provider's proxy, \"none\" forces a direct connection"
	m.proxy.SetStyles(com.Styles.TextInput)
	m.proxy.Prompt = "> "
	m.proxy.SetValue(account.ProxyURL)

	m.help = help.New()
	m.help.Styles = com.Styles.DialogHelpStyles()

	m.keyMap.Next = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next field"))
	m.keyMap.Prev = key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous field"))
	m.keyMap.Toggle = key.NewBinding(key.WithKeys("left", "right", "space"), key.WithHelp("←/→", "toggle enabled"))
	m.keyMap.Submit = key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "submit"))
	m.keyMap.Close = CloseKey

	return m
}

// ID implements Dialog.
func (m *AccountForm) ID() string {
	return AccountFormID
}

// HandleMsg implements [Dialog].
func (m *AccountForm) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case ActionAccountFormResult:
		m.submitting = false
		if msg.Err != nil {
			m.errMsg = msg.Err.Error()
			return nil
		}
		return ActionAccountSaved{ProviderID: msg.ProviderID}
	case tea.KeyPressMsg:
		if m.submitting {
			return nil
		}
		switch {
		case key.Matches(msg, m.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, m.keyMap.Next):
			m.advanceFocus(1)
		case key.Matches(msg, m.keyMap.Prev):
			m.advanceFocus(-1)
		case m.focus == accountFormFieldEnabled && key.Matches(msg, m.keyMap.Toggle):
			m.enabled = !m.enabled
		case key.Matches(msg, m.keyMap.Submit):
			return m.submit()
		default:
			return m.updateFocusedInput(msg)
		}
	case tea.PasteMsg:
		return m.updateFocusedInput(msg)
	}
	return nil
}

// advanceFocus moves focus by delta fields, wrapping around, and updates
// which text input (if any) is focused.
func (m *AccountForm) advanceFocus(delta int) {
	m.label.Blur()
	m.proxy.Blur()

	n := int(accountFormFieldCount)
	next := (int(m.focus) + delta%n + n) % n
	m.focus = accountFormField(next)

	switch m.focus {
	case accountFormFieldLabel:
		m.label.Focus()
	case accountFormFieldProxy:
		m.proxy.Focus()
	}
}

// updateFocusedInput forwards msg to whichever text input currently has
// focus. The Enabled field has no text input, so it's a no-op there.
func (m *AccountForm) updateFocusedInput(msg tea.Msg) Action {
	var cmd tea.Cmd
	switch m.focus {
	case accountFormFieldLabel:
		m.label, cmd = m.label.Update(msg)
	case accountFormFieldProxy:
		m.proxy, cmd = m.proxy.Update(msg)
	default:
		return nil
	}
	if cmd != nil {
		return ActionCmd{cmd}
	}
	return nil
}

// submit validates the fields and, if valid, arms the submitting state and
// returns the action the caller uses to kick off the async save. On
// validation failure it sets errMsg and returns nil so the dialog stays
// open without submitting.
//
// An active account cannot be disabled here: doing so would leave the
// provider pointing at a disabled account, which the rest of Sennit treats
// as unusable (see accounts.go's onSelect, which refuses to activate a
// disabled account). The user has to switch to a different account first —
// from the accounts list, not this form — before disabling this one.
func (m *AccountForm) submit() Action {
	proxy := strings.TrimSpace(m.proxy.Value())
	if err := proxyhttp.ValidateProxy(proxy); err != nil {
		m.errMsg = err.Error()
		return nil
	}
	if m.active && !m.enabled {
		m.errMsg = "Cannot disable the active account — activate a different account first"
		return nil
	}

	m.errMsg = ""
	m.submitting = true

	account := m.account
	account.Label = strings.TrimSpace(m.label.Value())
	account.ProxyURL = proxy
	account.Disabled = !m.enabled

	return ActionSubmitAccountForm{ProviderID: m.providerID, Account: account}
}

// Cursor returns the cursor position relative to the dialog. Each field's
// text input sits some number of rendered lines below the title — Draw()
// fills in m.fieldRow with that offset each frame, computed from what's
// actually drawn above the field rather than a hand-maintained constant.
func (m *AccountForm) Cursor() *tea.Cursor {
	var cur *tea.Cursor
	switch m.focus {
	case accountFormFieldLabel:
		cur = InputCursor(m.com.Styles, m.label.Cursor())
	case accountFormFieldProxy:
		cur = InputCursor(m.com.Styles, m.proxy.Cursor())
	default:
		return nil
	}
	if cur != nil {
		cur.Y += m.fieldRow[m.focus]
	}
	return cur
}

// Draw implements [Dialog].
func (m *AccountForm) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles

	m.Resize(area)
	innerWidth := m.InnerWidth()

	m.label.SetWidth(dialogInputTextWidth(t, m.label, innerWidth))
	m.proxy.SetWidth(dialogInputTextWidth(t, m.proxy, innerWidth))

	labelStyle := t.Dialog.SecondaryText
	inputStyle := t.Dialog.InputPrompt

	title := m.account.Label
	if title == "" {
		title = m.account.ID
	}
	rc := NewRenderContext(t, m.Width())
	rc.Title = "Edit " + title

	lines := 0
	addPart := func(part string) {
		rc.AddPart(part)
		lines += lipgloss.Height(part)
	}
	addField := func(field accountFormField, label string, input textinput.Model) {
		addPart(labelStyle.Render(label))
		m.fieldRow[field] = lines
		addPart(inputStyle.Render(input.View()))
	}

	m.fieldRow = make(map[accountFormField]int, 2)
	addField(accountFormFieldLabel, "Label (optional)", m.label)
	addField(accountFormFieldProxy, "Proxy (optional)", m.proxy)
	addPart(labelStyle.Render("Enabled") + "  " + m.enabledView())

	switch {
	case m.submitting:
		rc.AddPart(t.Dialog.SecondaryText.Render("Saving…"))
	case m.errMsg != "":
		rc.AddPart(t.Dialog.TitleError.Render(m.errMsg))
	}

	rc.Help = renderDialogHelp(t, &m.help, m, innerWidth)

	view := rc.Render()
	cur := m.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// enabledView renders the current Enabled value with toggle-hint arrows,
// highlighted when the Enabled field has focus.
func (m *AccountForm) enabledView() string {
	style := m.com.Styles.Dialog.SecondaryText
	if m.focus == accountFormFieldEnabled {
		style = m.com.Styles.Dialog.PrimaryText
	}
	value := "No"
	if m.enabled {
		value = "Yes"
	}
	return style.Render(fmt.Sprintf("‹ %s ›", value))
}

// ShortHelp implements [help.KeyMap].
func (m *AccountForm) ShortHelp() []key.Binding {
	h := []key.Binding{m.keyMap.Next}
	if m.focus == accountFormFieldEnabled {
		h = append(h, m.keyMap.Toggle)
	}
	return append(h, m.keyMap.Submit, m.keyMap.Close)
}

// FullHelp implements [help.KeyMap].
func (m *AccountForm) FullHelp() [][]key.Binding {
	return [][]key.Binding{m.ShortHelp()}
}
