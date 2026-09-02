package dialog

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/proxyhttp"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// ProviderSettingsID is the identifier for the provider settings dialog.
const ProviderSettingsID = "provider_settings"

const providerSettingsMaxWidth = 60

// providerSettingsField indexes this dialog's focusable fields. Which ones
// actually exist for a given provider is decided once, in
// NewProviderSettings, from accounts.CapabilitiesOf(providerID).RotateOn.
type providerSettingsField int

const (
	providerSettingsFieldProxy providerSettingsField = iota
	providerSettingsFieldEnabled
	// providerSettingsFieldThreshold exists only for accounts.RotateThreshold
	// providers (Codex: it reports remaining allowance).
	providerSettingsFieldThreshold
	// providerSettingsFieldCooldown exists only for accounts.RotateRateLimit
	// providers (everyone else: rotation triggers on HTTP 429).
	providerSettingsFieldCooldown
)

// ProviderSettings edits a provider's own settings: its base proxy (see
// the runtime provider's ConfiguredProxyURL, which every account's effective proxy
// is resolved against) and, where the provider supports it, automatic
// account rotation (see internal/providers/accounts and
// internal/config.RotationConfig). It does no IO itself — submitting
// returns [ActionSubmitProviderSettings] and the caller performs the save
// in a tea.Cmd, mirroring AccountForm.
//
// The runtime rotator consumes the stored settings. The account order field
// is deliberately not offered here yet; the list order accounts were added
// in is fine for now.
type ProviderSettings struct {
	Base
	com *common.Common

	providerID string
	caps       accounts.Capabilities

	proxy     textinput.Model
	enabled   bool
	threshold textinput.Model
	cooldown  textinput.Model

	// order is the account order the config already carried, kept verbatim
	// so submitting the form preserves it. The form does not offer the
	// field (see the type comment), and the save writes the whole rotation
	// object at once — without carrying it, saving any other setting would
	// silently discard an order the user had written by hand.
	order []string

	// fields is the ordered, provider-specific set of focusable fields —
	// built once in NewProviderSettings from caps.RotateOn. A RotateNever
	// provider ends up with just [providerSettingsFieldProxy].
	fields []providerSettingsField
	focus  int

	submitting bool
	errMsg     string

	fieldRow map[providerSettingsField]int

	help help.Model

	keyMap struct {
		Next   key.Binding
		Prev   key.Binding
		Toggle key.Binding
		Submit key.Binding
		Close  key.Binding
	}
}

var _ Dialog = (*ProviderSettings)(nil)

// NewProviderSettings creates the settings form for providerID, prefilled
// from its current config. Which fields it offers is driven entirely by
// accounts.CapabilitiesOf(providerID).RotateOn: a RotateNever provider gets
// only the proxy field; RotateThreshold adds the remaining-allowance
// threshold; RotateRateLimit adds the post-429 cooldown instead — never
// both, and never a field config validation would reject (see
// providerload's rotation validation).
func NewProviderSettings(com *common.Common, providerID string) *ProviderSettings {
	return newProviderSettings(com, providerID, accounts.CapabilitiesOf(providerID))
}

// newProviderSettings is NewProviderSettings with caps passed in rather
// than looked up, so tests can exercise the accounts.RotateNever branch
// directly — accounts.CapabilitiesOf never actually returns RotateNever
// for any provider in its current registry, but the field-hiding logic
// below still has to honor it if a provider registry entry ever does.
func newProviderSettings(com *common.Common, providerID string, caps accounts.Capabilities) *ProviderSettings {
	pc, _ := com.Config().Providers.Get(providerID)

	m := &ProviderSettings{
		Base:       NewBase(com, providerSettingsMaxWidth),
		com:        com,
		providerID: providerID,
		caps:       caps,
		fields:     []providerSettingsField{providerSettingsFieldProxy},
	}

	m.proxy = textinput.New()
	m.proxy.SetVirtualCursor(false)
	m.proxy.Placeholder = "Empty inherits HTTP_PROXY/HTTPS_PROXY, \"none\" forces a direct connection"
	m.proxy.SetStyles(com.Styles.TextInput)
	m.proxy.Prompt = "> "
	m.proxy.SetValue(pc.ProxyURL)
	m.proxy.Focus()

	if caps.RotateOn != accounts.RotateNever {
		m.fields = append(m.fields, providerSettingsFieldEnabled)
		m.enabled = pc.Rotation != nil && pc.Rotation.Enabled
		if pc.Rotation != nil {
			m.order = pc.Rotation.Order
		}

		switch caps.RotateOn {
		case accounts.RotateThreshold:
			m.fields = append(m.fields, providerSettingsFieldThreshold)
			m.threshold = textinput.New()
			m.threshold.SetVirtualCursor(false)
			m.threshold.Placeholder = fmt.Sprintf("1-99, default %d", accounts.DefaultMinRemainingPercent)
			m.threshold.SetStyles(com.Styles.TextInput)
			m.threshold.Prompt = "> "
			if pc.Rotation != nil && pc.Rotation.MinRemainingPercent != 0 {
				m.threshold.SetValue(strconv.Itoa(pc.Rotation.MinRemainingPercent))
			}
		case accounts.RotateRateLimit:
			m.fields = append(m.fields, providerSettingsFieldCooldown)
			m.cooldown = textinput.New()
			m.cooldown.SetVirtualCursor(false)
			m.cooldown.Placeholder = "e.g. 10m, default " + accounts.DefaultCooldown.String()
			m.cooldown.SetStyles(com.Styles.TextInput)
			m.cooldown.Prompt = "> "
			if pc.Rotation != nil {
				m.cooldown.SetValue(pc.Rotation.Cooldown)
			}
		}
	}

	m.help = help.New()
	m.help.Styles = com.Styles.DialogHelpStyles()

	m.keyMap.Next = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next field"))
	m.keyMap.Prev = key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous field"))
	m.keyMap.Toggle = key.NewBinding(key.WithKeys("left", "right", "space"), key.WithHelp("←/→", "toggle rotation"))
	m.keyMap.Submit = key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "submit"))
	m.keyMap.Close = CloseKey

	return m
}

// ID implements Dialog.
func (m *ProviderSettings) ID() string {
	return ProviderSettingsID
}

// currentField returns the field focus currently points at.
func (m *ProviderSettings) currentField() providerSettingsField {
	return m.fields[m.focus]
}

// HandleMsg implements [Dialog].
func (m *ProviderSettings) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case ActionProviderSettingsResult:
		m.submitting = false
		if msg.Err != nil {
			m.errMsg = msg.Err.Error()
			return nil
		}
		return ActionProviderSettingsSaved{ProviderID: msg.ProviderID}
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
		case m.currentField() == providerSettingsFieldEnabled && key.Matches(msg, m.keyMap.Toggle):
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

// advanceFocus moves focus by delta fields, wrapping around within
// m.fields, and updates which text input (if any) is focused.
func (m *ProviderSettings) advanceFocus(delta int) {
	m.proxy.Blur()
	m.threshold.Blur()
	m.cooldown.Blur()

	n := len(m.fields)
	m.focus = (m.focus + delta%n + n) % n

	switch m.currentField() {
	case providerSettingsFieldProxy:
		m.proxy.Focus()
	case providerSettingsFieldThreshold:
		m.threshold.Focus()
	case providerSettingsFieldCooldown:
		m.cooldown.Focus()
	}
}

// updateFocusedInput forwards msg to whichever text input currently has
// focus. Enabled has no text input, so it's a no-op there.
func (m *ProviderSettings) updateFocusedInput(msg tea.Msg) Action {
	var cmd tea.Cmd
	switch m.currentField() {
	case providerSettingsFieldProxy:
		m.proxy, cmd = m.proxy.Update(msg)
	case providerSettingsFieldThreshold:
		m.threshold, cmd = m.threshold.Update(msg)
	case providerSettingsFieldCooldown:
		m.cooldown, cmd = m.cooldown.Update(msg)
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
// The account order the config already held is carried through untouched
// (see the order field): the save writes the rotation object whole, so
// anything the form does not offer has to be preserved explicitly or it is
// lost.
//
// Rotation is left nil when the provider is accounts.RotateNever — there is
// nothing to save for it, and the caller (applyProviderDialogAction) skips
// the rotation write entirely in that case rather than persisting an empty
// object. The threshold/cooldown range and format checks mirror
// providerload's own validation exactly, so a mistyped value is caught here
// instead of round-tripping through a config reload to surface as a
// doctor problem.
func (m *ProviderSettings) submit() Action {
	proxy := strings.TrimSpace(m.proxy.Value())
	if err := proxyhttp.ValidateProxy(proxy); err != nil {
		m.errMsg = err.Error()
		return nil
	}

	var rotation *config.RotationConfig
	if m.caps.RotateOn != accounts.RotateNever {
		rotation = &config.RotationConfig{Enabled: m.enabled, Order: m.order}
		switch m.caps.RotateOn {
		case accounts.RotateThreshold:
			if raw := strings.TrimSpace(m.threshold.Value()); raw != "" {
				value, err := strconv.Atoi(raw)
				if err != nil || value < 1 || value > 99 {
					m.errMsg = "Threshold must be a whole number between 1 and 99"
					return nil
				}
				rotation.MinRemainingPercent = value
			}
		case accounts.RotateRateLimit:
			if raw := strings.TrimSpace(m.cooldown.Value()); raw != "" {
				d, err := time.ParseDuration(raw)
				if err != nil || d <= 0 {
					m.errMsg = "Cooldown must be a positive duration, e.g. 10m"
					return nil
				}
				rotation.Cooldown = raw
			}
		}
	}

	m.errMsg = ""
	m.submitting = true

	return ActionSubmitProviderSettings{ProviderID: m.providerID, Proxy: proxy, Rotation: rotation}
}

// Cursor returns the cursor position relative to the dialog. Each focusable
// field's text input sits some number of rendered lines below the title —
// Draw() fills in m.fieldRow with that offset each frame.
func (m *ProviderSettings) Cursor() *tea.Cursor {
	var cur *tea.Cursor
	switch m.currentField() {
	case providerSettingsFieldProxy:
		cur = InputCursor(m.com.Styles, m.proxy.Cursor())
	case providerSettingsFieldThreshold:
		cur = InputCursor(m.com.Styles, m.threshold.Cursor())
	case providerSettingsFieldCooldown:
		cur = InputCursor(m.com.Styles, m.cooldown.Cursor())
	default:
		return nil
	}
	if cur != nil {
		cur.Y += m.fieldRow[m.currentField()]
	}
	return cur
}

// Draw implements [Dialog].
func (m *ProviderSettings) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles

	m.Resize(area)
	innerWidth := m.InnerWidth()

	m.proxy.SetWidth(dialogInputTextWidth(t, m.proxy, innerWidth))
	switch m.caps.RotateOn {
	case accounts.RotateThreshold:
		m.threshold.SetWidth(dialogInputTextWidth(t, m.threshold, innerWidth))
	case accounts.RotateRateLimit:
		m.cooldown.SetWidth(dialogInputTextWidth(t, m.cooldown, innerWidth))
	}

	labelStyle := t.Dialog.SecondaryText
	inputStyle := t.Dialog.InputPrompt

	rc := NewRenderContext(t, m.Width())
	rc.Title = providerDisplayName(m.com, m.providerID) + " Settings"

	lines := 0
	addPart := func(part string) {
		rc.AddPart(part)
		lines += lipgloss.Height(part)
	}
	addField := func(field providerSettingsField, label string, input textinput.Model) {
		addPart(labelStyle.Render(label))
		m.fieldRow[field] = lines
		addPart(inputStyle.Render(input.View()))
	}

	m.fieldRow = make(map[providerSettingsField]int, len(m.fields))
	addField(providerSettingsFieldProxy, "Proxy (optional)", m.proxy)

	if m.caps.RotateOn != accounts.RotateNever {
		addPart(labelStyle.Render("Rotate accounts automatically") + "  " + m.enabledView())
		switch m.caps.RotateOn {
		case accounts.RotateThreshold:
			addPart(t.Dialog.SecondaryText.Render("Switches when the remaining limit drops below the threshold."))
			addField(providerSettingsFieldThreshold, "Remaining-allowance threshold, %", m.threshold)
		case accounts.RotateRateLimit:
			addPart(t.Dialog.SecondaryText.Render("Switches when the provider answers with a rate-limit error."))
			addField(providerSettingsFieldCooldown, "Cooldown after a rate limit", m.cooldown)
		}
	}

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
func (m *ProviderSettings) enabledView() string {
	style := m.com.Styles.Dialog.SecondaryText
	if m.currentField() == providerSettingsFieldEnabled {
		style = m.com.Styles.Dialog.PrimaryText
	}
	value := "No"
	if m.enabled {
		value = "Yes"
	}
	return style.Render(fmt.Sprintf("‹ %s ›", value))
}

// ShortHelp implements [help.KeyMap].
func (m *ProviderSettings) ShortHelp() []key.Binding {
	h := []key.Binding{m.keyMap.Next}
	if m.currentField() == providerSettingsFieldEnabled {
		h = append(h, m.keyMap.Toggle)
	}
	return append(h, m.keyMap.Submit, m.keyMap.Close)
}

// FullHelp implements [help.KeyMap].
func (m *ProviderSettings) FullHelp() [][]key.Binding {
	return [][]key.Binding{m.ShortHelp()}
}

var _ help.KeyMap = (*ProviderSettings)(nil)
