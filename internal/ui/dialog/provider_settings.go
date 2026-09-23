package dialog

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/proxyhttp"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/key"
	"github.com/rave-soft/sennit/internal/workspace"
)

// ProviderSettingsID is the identifier for the provider settings dialog.
const ProviderSettingsID = "provider_settings"

const providerSettingsMaxWidth = 60

// providerSettingsField indexes this dialog's focusable fields. Which ones
// actually exist for a given provider is decided once, in
// NewProviderSettings, from
// com.Workspace.AccountCapabilities(providerID).RotateOn.
type providerSettingsField int

const (
	providerSettingsFieldProxy providerSettingsField = iota
	providerSettingsFieldEnabled
	// providerSettingsFieldThreshold exists for providers that rotate on
	// a threshold (workspace.RotateThreshold and workspace.RotateBoth:
	// the latter also keeps its rate-limit fallback).
	providerSettingsFieldThreshold
	// providerSettingsFieldCooldown exists for providers that rotate on a
	// 429 (workspace.RotateRateLimit and workspace.RotateBoth).
	providerSettingsFieldCooldown
)

// providerSettingsAuthState describes the state of the provider's live
// credential - the one requests actually go out with - for a provider that
// uses OAuth tokens. It is read-only display state, not a focusable field.
//
// This is a provider-level dialog and this is the provider-level
// credential: which of several stored accounts is live, and what shape each
// of the others is in, is the accounts dialog's subject, not this one's.
type providerSettingsAuthState int

const (
	providerSettingsAuthUnknown providerSettingsAuthState = iota
	providerSettingsAuthOK
	providerSettingsAuthExpired
	providerSettingsAuthMissing
)

// providerSettingsAuthLoadedMsg carries the result of the async
// credential read kicked off by NewProviderSettings. It round-trips back
// into this same dialog's HandleMsg via the DialogAddressed mechanism, the
// same way ActionAccountsLoaded does.
type providerSettingsAuthLoadedMsg struct {
	providerID string
	state      providerSettingsAuthState
}

// DialogID implements [DialogAddressed].
func (providerSettingsAuthLoadedMsg) DialogID() string { return ProviderSettingsID }

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
	caps       workspace.AccountCapabilities

	proxy     textinput.Model
	enabled   bool
	threshold textinput.Model
	cooldown  textinput.Model

	// authState reflects the provider's live credential, populated by
	// loadAuthStateCmd. It is only meaningful for providers that use OAuth
	// tokens; API-key providers keep it at providerSettingsAuthUnknown.
	authState providerSettingsAuthState

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

	// canRefresh reports whether the provider is a custom one with a
	// base_url, the only kind a model refresh applies to.
	canRefresh bool
	// refreshing is set while a model refresh is in flight. Unlike
	// submitting, it still lets the dialog close.
	refreshing bool
	refreshMsg string

	help help.Model

	keyMap struct {
		Next    key.Binding
		Prev    key.Binding
		Toggle  key.Binding
		Submit  key.Binding
		Refresh key.Binding
		Close   key.Binding
	}
}

var _ Dialog = (*ProviderSettings)(nil)

// NewProviderSettings creates the settings form for providerID, prefilled
// from its current config. Which fields it offers is driven entirely by
// com.Workspace.AccountCapabilities(providerID).RotateOn: a RotateNever
// provider gets only the proxy field; RotateThreshold adds the
// remaining-allowance threshold; RotateRateLimit adds the post-429
// cooldown instead — never both, and never a field config validation
// would reject (see providerload's rotation validation).
//
// The returned tea.Cmd kicks off the async credential read that populates
// m.authState; run it alongside the dialog to have the auth badge visible
// on first frame.
func NewProviderSettings(com *common.Common, providerID string) (*ProviderSettings, tea.Cmd) {
	m := newProviderSettings(com, providerID, com.Workspace.AccountCapabilities(providerID))
	return m, m.loadAuthStateCmd()
}

// newProviderSettings is NewProviderSettings with caps passed in rather
// than looked up, so tests can exercise the workspace.RotateNever branch
// directly — the backing accounts.CapabilitiesOf never actually returns
// RotateNever for any provider in its current registry, but the
// field-hiding logic below still has to honor it if a provider registry
// entry ever does.
func newProviderSettings(com *common.Common, providerID string, caps workspace.AccountCapabilities) *ProviderSettings {
	pc, _ := com.Config().Providers.Get(providerID)

	m := &ProviderSettings{
		Base:       NewBase(com, providerSettingsMaxWidth),
		com:        com,
		providerID: providerID,
		caps:       caps,
		fields:     []providerSettingsField{providerSettingsFieldProxy},
		canRefresh: isCustomProvider(com, providerID, pc),
	}

	m.proxy = textinput.New()
	m.proxy.SetVirtualCursor(false)
	m.proxy.Placeholder = "Empty inherits HTTP_PROXY/HTTPS_PROXY, \"none\" forces a direct connection"
	m.proxy.SetStyles(com.Styles.TextInput)
	m.proxy.Prompt = "> "
	m.proxy.SetValue(pc.ProxyURL)
	m.proxy.Focus()

	if caps.RotateOn != workspace.RotateNever {
		m.fields = append(m.fields, providerSettingsFieldEnabled)
		m.enabled = pc.Rotation != nil && pc.Rotation.Enabled
		if pc.Rotation != nil {
			m.order = pc.Rotation.Order
		}

		switch caps.RotateOn {
		case workspace.RotateThreshold:
			m.fields = append(m.fields, providerSettingsFieldThreshold)
			m.threshold = textinput.New()
			m.threshold.SetVirtualCursor(false)
			m.threshold.Placeholder = fmt.Sprintf("1-99, default %d", workspace.DefaultMinRemainingPercent)
			m.threshold.SetStyles(com.Styles.TextInput)
			m.threshold.Prompt = "> "
			if pc.Rotation != nil && pc.Rotation.MinRemainingPercent != 0 {
				m.threshold.SetValue(strconv.Itoa(pc.Rotation.MinRemainingPercent))
			}
		case workspace.RotateRateLimit:
			m.fields = append(m.fields, providerSettingsFieldCooldown)
			m.cooldown = textinput.New()
			m.cooldown.SetVirtualCursor(false)
			m.cooldown.Placeholder = "e.g. 10m, default " + workspace.DefaultCooldown.String()
			m.cooldown.SetStyles(com.Styles.TextInput)
			m.cooldown.Prompt = "> "
			if pc.Rotation != nil {
				m.cooldown.SetValue(pc.Rotation.Cooldown)
			}
		case workspace.RotateBoth:
			m.fields = append(m.fields, providerSettingsFieldThreshold, providerSettingsFieldCooldown)
			m.threshold = textinput.New()
			m.threshold.SetVirtualCursor(false)
			m.threshold.Placeholder = fmt.Sprintf("1-99, default %d", workspace.DefaultMinRemainingPercent)
			m.threshold.SetStyles(com.Styles.TextInput)
			m.threshold.Prompt = "> "
			if pc.Rotation != nil && pc.Rotation.MinRemainingPercent != 0 {
				m.threshold.SetValue(strconv.Itoa(pc.Rotation.MinRemainingPercent))
			}
			m.cooldown = textinput.New()
			m.cooldown.SetVirtualCursor(false)
			m.cooldown.Placeholder = "e.g. 10m, default " + workspace.DefaultCooldown.String()
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
	m.keyMap.Refresh = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh models"))
	m.keyMap.Close = CloseKey

	return m
}

// loadAuthStateCmd reads the provider's live credential off the Update
// loop. It reports nothing (providerSettingsAuthUnknown, and no badge) for
// a provider that is not configured at all, and for one authenticating
// with an API key, which has no expiry to report on. com and providerID
// are captured by value so the closure doesn't race with the dialog being
// mutated concurrently.
func (m *ProviderSettings) loadAuthStateCmd() tea.Cmd {
	com := m.com
	providerID := m.providerID
	return func() tea.Msg {
		return providerSettingsAuthLoadedMsg{
			providerID: providerID,
			state:      providerAuthState(com, providerID),
		}
	}
}

// providerAuthState classifies the provider's live credential.
func providerAuthState(com *common.Common, providerID string) providerSettingsAuthState {
	pc, ok := com.Config().RuntimeProvider(providerID)
	switch {
	case !ok:
		return providerSettingsAuthUnknown
	case pc.OAuthToken != nil:
		if pc.OAuthToken.IsExpired() {
			return providerSettingsAuthExpired
		}
		return providerSettingsAuthOK
	case pc.APIKey != "":
		// An API key carries no expiry, so there is nothing to report
		// that the user cannot already see in the settings themselves.
		return providerSettingsAuthUnknown
	default:
		return providerSettingsAuthMissing
	}
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
	case providerSettingsAuthLoadedMsg:
		if msg.providerID != m.providerID {
			return nil
		}
		m.authState = msg.state
		return nil
	case ActionProviderSettingsResult:
		m.submitting = false
		if msg.Err != nil {
			m.errMsg = msg.Err.Error()
			return nil
		}
		return ActionProviderSettingsSaved{ProviderID: msg.ProviderID}
	case ActionRefreshModelsResult:
		if msg.ProviderID != m.providerID {
			return nil
		}
		m.refreshing = false
		m.refreshMsg = ""
		err := msg.Err
		if err == nil && len(msg.Results) > 0 {
			err = msg.Results[0].Err
		}
		if err != nil {
			m.errMsg = err.Error()
			return nil
		}
		m.errMsg = ""
		m.refreshMsg = formatModelRefreshStatus(msg.Results)
		return nil
	case tea.KeyPressMsg:
		if m.submitting {
			return nil
		}
		if m.refreshing {
			if key.Matches(msg, m.keyMap.Close) {
				return ActionClose{}
			}
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
		case m.refreshAvailable() && key.Matches(msg, m.keyMap.Refresh):
			m.refreshing = true
			m.errMsg = ""
			m.refreshMsg = ""
			return ActionRefreshModels{ProviderID: m.providerID}
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
// Rotation is left nil when the provider is workspace.RotateNever — there is
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
	if m.caps.RotateOn != workspace.RotateNever {
		rotation = &config.RotationConfig{Enabled: m.enabled, Order: m.order}
		switch m.caps.RotateOn {
		case workspace.RotateThreshold:
			if raw := strings.TrimSpace(m.threshold.Value()); raw != "" {
				value, err := strconv.Atoi(raw)
				if err != nil || value < 1 || value > 99 {
					m.errMsg = "Threshold must be a whole number between 1 and 99"
					return nil
				}
				rotation.MinRemainingPercent = value
			}
		case workspace.RotateRateLimit:
			if raw := strings.TrimSpace(m.cooldown.Value()); raw != "" {
				d, err := time.ParseDuration(raw)
				if err != nil || d <= 0 {
					m.errMsg = "Cooldown must be a positive duration, e.g. 10m"
					return nil
				}
				rotation.Cooldown = raw
			}
		case workspace.RotateBoth:
			if raw := strings.TrimSpace(m.threshold.Value()); raw != "" {
				value, err := strconv.Atoi(raw)
				if err != nil || value < 1 || value > 99 {
					m.errMsg = "Threshold must be a whole number between 1 and 99"
					return nil
				}
				rotation.MinRemainingPercent = value
			}
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

// Cursor returns the cursor position relative to the dialog by finding
// the focused field's prompt in the rendered view. Deriving the offset
// from style getters (the old fieldRow approach) drifts from what is
// actually rendered when the layout includes blank-line margins around
// InputPrompt-styled inputs, which added a phantom row that pushed the
// cursor one line too low. See [promptCursor] for how the search works.
//
// All three inputs share the same prompt ("> "), so the field is picked by
// position: inputIndex counts the inputs rendered before the focused one,
// and Draw renders them in m.fields order, which is the order they are
// focused in. Matching on the field's value or placeholder instead would
// be ambiguous the moment two fields held the same text.
//
// view is the full rendered dialog string (what Draw renders); an empty
// one, or a focused field with no input, drops the cursor rather than
// guessing at it.
func (m *ProviderSettings) Cursor(view string) *tea.Cursor {
	input, ok := m.focusedInput()
	if !ok {
		return nil
	}
	return promptCursor(view, input.Prompt, m.inputIndex(), input.Cursor())
}

// focusedInput returns the text input of the field that currently has
// focus. The Enabled field is not a text input, so it reports false.
func (m *ProviderSettings) focusedInput() (textinput.Model, bool) {
	switch m.currentField() {
	case providerSettingsFieldProxy:
		return m.proxy, true
	case providerSettingsFieldThreshold:
		return m.threshold, true
	case providerSettingsFieldCooldown:
		return m.cooldown, true
	default:
		return textinput.Model{}, false
	}
}

// inputIndex reports how many text inputs Draw renders before the focused
// one. Draw walks m.fields in order and the Enabled field draws no input,
// so counting the text-input fields ahead of the focused one gives the
// focused input's position among the prompt lines in the rendered view.
func (m *ProviderSettings) inputIndex() int {
	n := 0
	for _, f := range m.fields {
		if f == m.currentField() {
			break
		}
		if f != providerSettingsFieldEnabled {
			n++
		}
	}
	return n
}

// Draw implements [Dialog].
func (m *ProviderSettings) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles

	m.Resize(area)
	innerWidth := m.InnerWidth()

	m.proxy.SetWidth(dialogInputTextWidth(t, m.proxy, innerWidth))
	switch m.caps.RotateOn {
	case workspace.RotateThreshold:
		m.threshold.SetWidth(dialogInputTextWidth(t, m.threshold, innerWidth))
	case workspace.RotateRateLimit:
		m.cooldown.SetWidth(dialogInputTextWidth(t, m.cooldown, innerWidth))
	case workspace.RotateBoth:
		m.threshold.SetWidth(dialogInputTextWidth(t, m.threshold, innerWidth))
		m.cooldown.SetWidth(dialogInputTextWidth(t, m.cooldown, innerWidth))
	}

	labelStyle := t.Dialog.SecondaryText
	inputStyle := t.Dialog.InputPrompt

	rc := NewRenderContext(t, m.Width())
	rc.Title = providerDisplayName(m.com, m.providerID) + " Settings"

	addPart := func(part string) {
		rc.AddPart(part)
	}
	addField := func(label string, input textinput.Model) {
		addPart(labelStyle.Render(label))
		addPart(inputStyle.Render(input.View()))
	}

	addField("Proxy (optional)", m.proxy)

	if m.caps.RotateOn != workspace.RotateNever {
		addPart(labelStyle.Render("Rotate accounts automatically") + "  " + m.enabledView())
		switch m.caps.RotateOn {
		case workspace.RotateThreshold:
			addPart(t.Dialog.SecondaryText.Render("Switches when the remaining limit drops below the threshold."))
			addField("Remaining-allowance threshold, %", m.threshold)
		case workspace.RotateRateLimit:
			addPart(t.Dialog.SecondaryText.Render("Switches when the provider answers with a rate-limit error."))
			addField("Cooldown after a rate limit", m.cooldown)
		case workspace.RotateBoth:
			addPart(t.Dialog.SecondaryText.Render("Switches when the remaining limit drops below the threshold, or when the provider answers with a rate-limit error."))
			addField("Remaining-allowance threshold, %", m.threshold)
			addField("Cooldown after a rate limit", m.cooldown)
		}
	}

	if badge := m.authBadge(); badge != "" {
		rc.AddPart(badge)
	}

	switch {
	case m.submitting:
		rc.AddPart(t.Dialog.SecondaryText.Render("Saving…"))
	case m.refreshing:
		rc.AddPart(t.Dialog.SecondaryText.Render("Refreshing models…"))
	case m.errMsg != "":
		rc.AddPart(t.Dialog.TitleError.Render(m.errMsg))
	case m.refreshMsg != "":
		rc.AddPart(t.Dialog.SecondaryText.Render(m.refreshMsg))
	}

	rc.Help = renderDialogHelp(t, &m.help, m, innerWidth)

	view := rc.Render()
	cur := m.Cursor(view)
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// isCustomProvider reports whether providerID is a custom provider with a
// base_url: not in the catalog, so its models come from discovery.
func isCustomProvider(com *common.Common, providerID string, pc config.ProviderConfig) bool {
	if pc.BaseURL == "" {
		return false
	}
	for _, p := range com.Workspace.KnownProviders() {
		if string(p.ID) == providerID {
			return false
		}
	}
	return true
}

// refreshAvailable reports whether the refresh key applies now. It is
// bound to the Enabled field because the text fields would take "r" as
// input.
func (m *ProviderSettings) refreshAvailable() bool {
	return m.canRefresh && m.currentField() == providerSettingsFieldEnabled
}

// formatModelRefreshStatus summarizes the refresh of the one provider the
// dialog requested. A failed refresh never reaches it: HandleMsg shows the
// error instead.
func formatModelRefreshStatus(results []workspace.ModelRefreshResult) string {
	if len(results) == 0 {
		return "No models were eligible for refresh"
	}
	result := results[0]
	if result.Skipped {
		return "Refresh skipped: " + result.SkipReason
	}
	return fmt.Sprintf("Refreshed: %d models (+%d new, -%d removed)", result.Models, result.Added, result.Removed)
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

// authBadge renders the provider's credential state as a short status
// line. It returns "" when there is nothing to report (authState stays
// providerSettingsAuthUnknown), so the badge is invisible for API-key
// providers.
func (m *ProviderSettings) authBadge() string {
	t := m.com.Styles
	label, style := m.authStateLabel()
	if label == "" {
		return ""
	}
	return t.Dialog.SecondaryText.Render("Credentials: ") + style.Render(label)
}

// authStateLabel returns the display text and style for m.authState.
// An empty label means "do not render".
func (m *ProviderSettings) authStateLabel() (string, lipgloss.Style) {
	t := m.com.Styles
	switch m.authState {
	case providerSettingsAuthOK:
		return "signed in", t.Dialog.OAuth.Success
	case providerSettingsAuthExpired:
		return "token expired", t.Dialog.OAuth.ErrorText
	case providerSettingsAuthMissing:
		return "not signed in", t.Dialog.OAuth.ErrorText
	default:
		return "", lipgloss.NewStyle()
	}
}

// ShortHelp implements [help.KeyMap].
func (m *ProviderSettings) ShortHelp() []key.Binding {
	h := []key.Binding{m.keyMap.Next}
	if m.currentField() == providerSettingsFieldEnabled {
		h = append(h, m.keyMap.Toggle)
	}
	if m.refreshAvailable() {
		h = append(h, m.keyMap.Refresh)
	}
	return append(h, m.keyMap.Submit, m.keyMap.Close)
}

// FullHelp implements [help.KeyMap].
func (m *ProviderSettings) FullHelp() [][]key.Binding {
	return [][]key.Binding{m.ShortHelp()}
}

var _ help.KeyMap = (*ProviderSettings)(nil)
