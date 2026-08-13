package dialog

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/discover"
	"github.com/rave-soft/braid/internal/ui/common"
)

// ProviderFormID is the identifier for the custom provider form dialog.
const ProviderFormID = "provider_form"

const providerFormMaxWidth = 60

// providerFormField indexes the focusable fields, in tab order.
type providerFormField int

const (
	providerFormFieldID providerFormField = iota
	providerFormFieldBaseURL
	providerFormFieldType
	providerFormFieldAPIKey
	providerFormFieldCount
)

// ProviderForm is a dialog for creating a custom (non-catalog) provider: ID,
// base URL, provider type, and an optional API key. It does no IO itself —
// submitting returns [ActionSubmitCustomProvider] and the caller (ui.go)
// does the async save-and-discover work in a tea.Cmd. The result rounds
// back as [ActionCustomProviderResult] (see actions.go's doc comment on
// that type for how the round-trip reaches HandleMsg below).
type ProviderForm struct {
	Base
	com *common.Common

	id      textinput.Model
	baseURL textinput.Model
	apiKey  textinput.Model
	types   []string
	typeIdx int

	focus      providerFormField
	submitting bool
	errMsg     string

	// fieldRow holds each text field's row offset (relative to the shared
	// InputCursor baseline), recomputed every Draw() from the actual
	// rendered heights of the parts above it — see Draw() and Cursor().
	fieldRow map[providerFormField]int

	help help.Model

	keyMap struct {
		Next   key.Binding
		Prev   key.Binding
		Left   key.Binding
		Right  key.Binding
		Submit key.Binding
		Close  key.Binding
	}
}

var _ Dialog = (*ProviderForm)(nil)

// NewProviderForm creates a new custom provider form dialog.
func NewProviderForm(com *common.Common) *ProviderForm {
	m := &ProviderForm{Base: NewBase(com, providerFormMaxWidth), com: com}

	m.id = textinput.New()
	m.id.SetVirtualCursor(false)
	m.id.Placeholder = "my-provider"
	m.id.SetStyles(com.Styles.TextInput)
	m.id.Prompt = "> "
	m.id.Focus()

	m.baseURL = textinput.New()
	m.baseURL.SetVirtualCursor(false)
	m.baseURL.Placeholder = "https://api.example.com/v1"
	m.baseURL.SetStyles(com.Styles.TextInput)
	m.baseURL.Prompt = "> "

	m.apiKey = textinput.New()
	m.apiKey.SetVirtualCursor(false)
	m.apiKey.Placeholder = "Optional"
	m.apiKey.SetStyles(com.Styles.TextInput)
	m.apiKey.Prompt = "> "

	m.types = providerFormTypes()
	m.typeIdx = max(0, slices.Index(m.types, string(catwalk.TypeOpenAICompat)))

	m.help = help.New()
	m.help.Styles = com.Styles.DialogHelpStyles()

	m.keyMap.Next = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next field"))
	m.keyMap.Prev = key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous field"))
	m.keyMap.Left = key.NewBinding(key.WithKeys("left"), key.WithHelp("←/→", "change type"))
	m.keyMap.Right = key.NewBinding(key.WithKeys("right"), key.WithHelp("→", "change type"))
	m.keyMap.Submit = key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "submit"))
	m.keyMap.Close = CloseKey

	return m
}

// providerFormTypes returns the valid `type` values for a custom provider:
// catwalk's own catalog types union the custom provider types that have a
// registered discover.Enricher (ollama, omlx, litellm, llamacpp, lmstudio).
func providerFormTypes() []string {
	known := catwalk.KnownProviderTypes()
	types := make([]string, 0, len(known)+4)
	for _, t := range known {
		types = append(types, string(t))
	}
	types = append(types, discover.RegisteredProviderTypes()...)
	return types
}

// ID implements Dialog.
func (m *ProviderForm) ID() string {
	return ProviderFormID
}

// HandleMsg implements [Dialog].
func (m *ProviderForm) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case ActionCustomProviderResult:
		m.submitting = false
		if msg.Err != nil {
			m.errMsg = msg.Err.Error()
			return nil
		}
		return ActionProviderConfigured{ProviderID: msg.ProviderID}
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
		case m.focus == providerFormFieldType && key.Matches(msg, m.keyMap.Left):
			m.cycleType(-1)
		case m.focus == providerFormFieldType && key.Matches(msg, m.keyMap.Right):
			m.cycleType(1)
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
func (m *ProviderForm) advanceFocus(delta int) {
	m.id.Blur()
	m.baseURL.Blur()
	m.apiKey.Blur()

	n := int(providerFormFieldCount)
	next := (int(m.focus) + delta%n + n) % n
	m.focus = providerFormField(next)

	switch m.focus {
	case providerFormFieldID:
		m.id.Focus()
	case providerFormFieldBaseURL:
		m.baseURL.Focus()
	case providerFormFieldAPIKey:
		m.apiKey.Focus()
	}
}

// cycleType moves the selected provider type by delta, wrapping around.
func (m *ProviderForm) cycleType(delta int) {
	if len(m.types) == 0 {
		return
	}
	n := len(m.types)
	m.typeIdx = (m.typeIdx + delta%n + n) % n
}

// updateFocusedInput forwards msg to whichever text input currently has
// focus. The type field has no text input, so it's a no-op there.
func (m *ProviderForm) updateFocusedInput(msg tea.Msg) Action {
	var cmd tea.Cmd
	switch m.focus {
	case providerFormFieldID:
		m.id, cmd = m.id.Update(msg)
	case providerFormFieldBaseURL:
		m.baseURL, cmd = m.baseURL.Update(msg)
	case providerFormFieldAPIKey:
		m.apiKey, cmd = m.apiKey.Update(msg)
	default:
		return nil
	}
	if cmd != nil {
		return ActionCmd{cmd}
	}
	return nil
}

// submit validates the required fields and, if valid, arms the submitting
// state and returns the action the caller uses to kick off the async
// save-and-discover work. On validation failure it sets errMsg and returns
// nil so the dialog stays open without submitting.
func (m *ProviderForm) submit() Action {
	id := strings.TrimSpace(m.id.Value())
	baseURL := strings.TrimSpace(m.baseURL.Value())

	switch {
	case id == "":
		m.errMsg = "Provider ID is required"
		return nil
	case baseURL == "":
		m.errMsg = "Base URL is required"
		return nil
	}

	m.errMsg = ""
	m.submitting = true

	var providerType string
	if m.typeIdx >= 0 && m.typeIdx < len(m.types) {
		providerType = m.types[m.typeIdx]
	}

	return ActionSubmitCustomProvider{
		ID:      id,
		BaseURL: baseURL,
		Type:    providerType,
		APIKey:  m.apiKey.Value(),
	}
}

// Cursor returns the cursor position relative to the dialog. Each field's
// text input sits some number of rendered lines below the title — Draw()
// fills in m.fieldRow with that offset each frame, computed from what's
// actually drawn above the field rather than a hand-maintained constant.
func (m *ProviderForm) Cursor() *tea.Cursor {
	var cur *tea.Cursor
	switch m.focus {
	case providerFormFieldID:
		cur = InputCursor(m.com.Styles, m.id.Cursor())
	case providerFormFieldBaseURL:
		cur = InputCursor(m.com.Styles, m.baseURL.Cursor())
	case providerFormFieldAPIKey:
		cur = InputCursor(m.com.Styles, m.apiKey.Cursor())
	default:
		return nil
	}
	if cur != nil {
		cur.Y += m.fieldRow[m.focus]
	}
	return cur
}

// Draw implements [Dialog].
func (m *ProviderForm) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles

	m.Resize(area)
	innerWidth := m.InnerWidth()

	m.id.SetWidth(dialogInputTextWidth(t, m.id, innerWidth))
	m.baseURL.SetWidth(dialogInputTextWidth(t, m.baseURL, innerWidth))
	m.apiKey.SetWidth(dialogInputTextWidth(t, m.apiKey, innerWidth))

	labelStyle := t.Dialog.SecondaryText
	inputStyle := t.Dialog.InputPrompt

	rc := NewRenderContext(t, m.Width())
	rc.Title = "Custom Provider"

	// lines tracks the rendered height of the parts added so far (they all
	// sit on their own lines below the title, per rc.Gap==0). inputStyle
	// adds its own blank margin lines above and below each input, so a
	// field's row can't be a fixed per-field constant — it has to be
	// derived from what was actually drawn above it.
	lines := 0
	addPart := func(part string) {
		rc.AddPart(part)
		lines += lipgloss.Height(part)
	}
	addField := func(field providerFormField, label string, input textinput.Model) {
		addPart(labelStyle.Render(label))
		m.fieldRow[field] = lines
		addPart(inputStyle.Render(input.View()))
	}

	m.fieldRow = make(map[providerFormField]int, 3)
	addField(providerFormFieldID, "Provider ID", m.id)
	addField(providerFormFieldBaseURL, "Base URL", m.baseURL)
	addPart(labelStyle.Render("Type") + "  " + m.typeView())
	addField(providerFormFieldAPIKey, "API Key (optional)", m.apiKey)

	switch {
	case m.submitting:
		rc.AddPart(t.Dialog.SecondaryText.Render("Saving and discovering models…"))
	case m.errMsg != "":
		rc.AddPart(t.Dialog.TitleError.Render(m.errMsg))
	}

	rc.Help = renderDialogHelp(t, &m.help, m, innerWidth)

	view := rc.Render()
	cur := m.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// typeView renders the current type value with cycle-hint arrows,
// highlighted when the type field has focus.
func (m *ProviderForm) typeView() string {
	style := m.com.Styles.Dialog.SecondaryText
	if m.focus == providerFormFieldType {
		style = m.com.Styles.Dialog.PrimaryText
	}
	if len(m.types) == 0 {
		return style.Render("(none)")
	}
	return style.Render(fmt.Sprintf("‹ %s ›", m.types[m.typeIdx]))
}

// ShortHelp implements [help.KeyMap].
func (m *ProviderForm) ShortHelp() []key.Binding {
	h := []key.Binding{m.keyMap.Next}
	if m.focus == providerFormFieldType {
		h = append(h, m.keyMap.Left, m.keyMap.Right)
	}
	return append(h, m.keyMap.Submit, m.keyMap.Close)
}

// FullHelp implements [help.KeyMap].
func (m *ProviderForm) FullHelp() [][]key.Binding {
	return [][]key.Binding{m.ShortHelp()}
}
