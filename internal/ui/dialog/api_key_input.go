package dialog

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/exp/charmtone"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
)

type APIKeyInputState int

const (
	APIKeyInputStateInitial APIKeyInputState = iota
	APIKeyInputStateVerifying
	APIKeyInputStateVerified
	APIKeyInputStateError
)

// APIKeyInputID is the identifier for the model selection dialog.
const APIKeyInputID = "api_key_input"

// APIKeyInput represents a model selection dialog.
type APIKeyInput struct {
	Base
	com          *common.Common
	isOnboarding bool

	provider catwalk.Provider
	// model is nil when this dialog is authenticating a provider outside
	// the model-switch flow (see NewProviders/ActionConfigureProvider) —
	// there's no model selection to carry forward, so saveKeyAndContinue
	// returns ActionProviderConfigured instead of ActionSelectModel.
	model *config.SelectedModel

	state APIKeyInputState

	keyMap struct {
		Submit key.Binding
		Close  key.Binding
	}
	input   textinput.Model
	spinner spinner.Model
	help    help.Model
}

var _ Dialog = (*APIKeyInput)(nil)

// NewAPIKeyInput creates a new Models dialog.
func NewAPIKeyInput(
	com *common.Common,
	isOnboarding bool,
	provider catwalk.Provider,
	model *config.SelectedModel,
) (*APIKeyInput, tea.Cmd) {
	t := com.Styles

	m := APIKeyInput{Base: NewBase(com, 60)}
	m.com = com
	m.isOnboarding = isOnboarding
	m.provider = provider
	m.model = model

	m.input = textinput.New()
	m.input.SetVirtualCursor(false)
	m.input.Placeholder = "Enter your API key..."
	m.input.SetStyles(com.Styles.TextInput)
	m.input.Focus()

	m.spinner = spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(t.Dialog.APIKey.Spinner),
	)

	m.help = help.New()
	m.help.Styles = t.DialogHelpStyles()

	m.keyMap.Submit = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "submit"),
	)
	m.keyMap.Close = CloseKey

	return &m, nil
}

// ID implements Dialog.
func (m *APIKeyInput) ID() string {
	return APIKeyInputID
}

// HandleMsg implements [Dialog].
func (m *APIKeyInput) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case ActionAPIKeySaved:
		if msg.Err != nil {
			return ActionCmd{util.ReportError(fmt.Errorf("failed to save API key: %w", msg.Err))}
		}
		if m.model == nil {
			return ActionProviderConfigured{ProviderID: string(m.provider.ID)}
		}
		return ActionSelectModel{
			Provider: m.provider,
			Model:    *m.model,
		}
	case ActionChangeAPIKeyState:
		m.state = msg.State
		switch m.state {
		case APIKeyInputStateVerifying:
			cmd := tea.Batch(m.spinner.Tick, m.verifyAPIKeyCmd())
			return ActionCmd{cmd}
		}
	case spinner.TickMsg:
		switch m.state {
		case APIKeyInputStateVerifying:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			if cmd != nil {
				return ActionCmd{cmd}
			}
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, m.keyMap.Close):
			// Checked before the "verifying absorbs everything" case below
			// so esc still gets the person out mid-verify — previously the
			// only way out of a stuck verify was to restart.
			switch m.state {
			case APIKeyInputStateVerified:
				return ActionCmd{m.saveAPIKeyCmd()}
			default:
				return ActionClose{}
			}
		case m.state == APIKeyInputStateVerifying:
			// do nothing
		case key.Matches(msg, m.keyMap.Submit):
			switch m.state {
			case APIKeyInputStateInitial, APIKeyInputStateError:
				return ActionChangeAPIKeyState{State: APIKeyInputStateVerifying}
			case APIKeyInputStateVerified:
				return ActionCmd{m.saveAPIKeyCmd()}
			}
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			if cmd != nil {
				return ActionCmd{cmd}
			}
		}
	case tea.PasteMsg:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if cmd != nil {
			return ActionCmd{cmd}
		}
	}
	return nil
}

// Draw implements [Dialog].
func (m *APIKeyInput) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles

	m.Resize(area)
	innerWidth := m.InnerWidth() - 2
	m.input.SetWidth(max(0, innerWidth-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1)) // (1) cursor padding

	textStyle := t.Dialog.SecondaryText
	dialogStyle := t.Dialog.View.Width(m.Width())
	inputStyle := t.Dialog.InputPrompt
	helpView := renderDialogHelp(t, &m.help, m, m.Width()-dialogStyle.GetHorizontalFrameSize())

	m.input.Prompt = m.spinner.View()

	content := strings.Join([]string{
		m.headerView(),
		inputStyle.Render(m.inputView()),
		textStyle.Render("This will be written in your global configuration:"),
		textStyle.Render(config.GlobalConfigData()),
		"",
		helpView,
	}, "\n")

	cur := m.Cursor()

	if m.isOnboarding {
		view := content
		cur = adjustOnboardingInputCursor(t, cur)
		DrawOnboardingCursor(scr, area, view, cur)
	} else {
		view := dialogStyle.Render(content)
		DrawCenterCursor(scr, area, view, cur)
	}
	return cur
}

func (m *APIKeyInput) headerView() string {
	var (
		t           = m.com.Styles
		titleStyle  = t.Dialog.Title
		textStyle   = t.Dialog.PrimaryText
		dialogStyle = t.Dialog.View.Width(m.Width())
	)
	if m.isOnboarding {
		return textStyle.Render(m.dialogTitle())
	}
	headerOffset := titleStyle.GetHorizontalFrameSize() + dialogStyle.GetHorizontalFrameSize()
	return common.DialogTitle(t, titleStyle.Render(m.dialogTitle()), m.width-headerOffset, m.com.Styles.Dialog.TitleGradFromColor, m.com.Styles.Dialog.TitleGradToColor)
}

func (m *APIKeyInput) dialogTitle() string {
	var (
		t           = m.com.Styles
		textStyle   = t.Dialog.TitleText
		errorStyle  = t.Dialog.TitleError
		accentStyle = t.Dialog.TitleAccent
	)
	switch m.state {
	case APIKeyInputStateInitial:
		return textStyle.Render("Enter your ") + accentStyle.Render(fmt.Sprintf("%s Key", m.provider.Name)) + textStyle.Render(".")
	case APIKeyInputStateVerifying:
		return textStyle.Render("Verifying your ") + accentStyle.Render(fmt.Sprintf("%s Key", m.provider.Name)) + textStyle.Render("...")
	case APIKeyInputStateVerified:
		return accentStyle.Render(fmt.Sprintf("%s Key", m.provider.Name)) + textStyle.Render(" validated.")
	case APIKeyInputStateError:
		return errorStyle.Render("Invalid ") + accentStyle.Render(fmt.Sprintf("%s Key", m.provider.Name)) + errorStyle.Render(". Try again?")
	}
	return ""
}

func (m *APIKeyInput) inputView() string {
	t := m.com.Styles

	switch m.state {
	case APIKeyInputStateInitial:
		m.input.Prompt = "> "
		m.input.SetStyles(t.TextInput)
		m.input.Focus()
	case APIKeyInputStateVerifying:
		ts := t.TextInput
		ts.Blurred.Prompt = ts.Focused.Prompt

		m.input.Prompt = m.spinner.View()
		m.input.SetStyles(ts)
		m.input.Blur()
	case APIKeyInputStateVerified:
		ts := t.TextInput
		ts.Blurred.Prompt = ts.Focused.Prompt

		m.input.Prompt = styles.CheckIcon + " "
		m.input.SetStyles(ts)
		m.input.Blur()
	case APIKeyInputStateError:
		ts := t.TextInput
		ts.Focused.Prompt = ts.Focused.Prompt.Foreground(charmtone.Cherry)

		m.input.Prompt = styles.LSPErrorIcon + " "
		m.input.SetStyles(ts)
		m.input.Focus()
	}
	return m.input.View()
}

// Cursor returns the cursor position relative to the dialog.
func (m *APIKeyInput) Cursor() *tea.Cursor {
	return InputCursor(m.com.Styles, m.input.Cursor())
}

// FullHelp returns the full help view.
func (m *APIKeyInput) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{
			m.keyMap.Submit,
			m.keyMap.Close,
		},
	}
}

// ShortHelp returns the full help view.
func (m *APIKeyInput) ShortHelp() []key.Binding {
	return []key.Binding{
		m.keyMap.Submit,
		m.keyMap.Close,
	}
}

// verifyAPIKeyCmd snapshots the current provider/key state in Update and
// does the actual verification off the Update goroutine, the same pattern
// as saveAPIKeyCmd below and OAuthCodex.startPolling: the returned closure
// touches only its captured copies, never m, so it's safe to run
// concurrently with future Update calls that mutate m.input.
//
// The verification itself goes through com.Workspace.VerifyProviderAPIKey
// rather than building a provider here: only the workspace knows how the
// agent would actually construct this provider (proxy, extra headers,
// account rotation, catalog base URL), and a dialog-built stand-in could
// pass while the real provider would fail, or vice versa. There is no
// artificial minimum duration here (the previous version slept in the
// command to hold the spinner visible for 750ms): the workspace call is a
// real network round trip, so the spinner already has something to show
// for the time it's up.
func (m *APIKeyInput) verifyAPIKeyCmd() tea.Cmd {
	ws := m.com.Workspace
	ctx := m.com.Context()
	providerID := string(m.provider.ID)
	apiKey := m.input.Value()
	return func() tea.Msg {
		err := ws.VerifyProviderAPIKey(ctx, providerID, apiKey)
		if err == nil {
			return ActionChangeAPIKeyState{APIKeyInputStateVerified}
		}
		return ActionChangeAPIKeyState{APIKeyInputStateError}
	}
}

// saveAPIKeyCmd persists the entered key off the Update goroutine and
// reports the outcome as [ActionAPIKeySaved], which HandleMsg turns into
// the actual follow-up action (see that case above).
func (m *APIKeyInput) saveAPIKeyCmd() tea.Cmd {
	ws := m.com.Workspace
	providerID := string(m.provider.ID)
	apiKey := m.input.Value()
	return func() tea.Msg {
		return ActionAPIKeySaved{Err: ws.SetProviderAPIKey(config.ScopeGlobal, providerID, apiKey)}
	}
}
