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
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/pkg/browser"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

type OAuthProvider interface {
	name() string
	initiateAuth() tea.Msg
	startPolling(deviceCode string, expiresIn int) tea.Cmd
	stopPolling() tea.Msg
}

// OAuthState represents the current state of the device flow.
type OAuthState int

const (
	OAuthStateInitializing OAuthState = iota
	OAuthStateDisplay
	OAuthStateSuccess
	OAuthStateSaving
	OAuthStateError
	// OAuthStateProxy asks for a proxy before the flow starts. Only
	// providers that opt in (see oauthProxyConfigurer) ever reach it —
	// for a provider users can only reach through a proxy, a sign-in
	// that started first would just fail.
	OAuthStateProxy
)

// OAuthID is the identifier for the model selection dialog.
const OAuthID = "oauth"

// OAuth handles the OAuth flow authentication.
type OAuth struct {
	com          *common.Common
	isOnboarding bool
	Base

	provider      catwalk.Provider
	model         *config.SelectedModel
	oAuthProvider OAuthProvider

	State OAuthState

	spinner spinner.Model
	help    help.Model
	keyMap  struct {
		Copy    key.Binding
		CopyURL key.Binding
		Submit  key.Binding
		Close   key.Binding
	}

	// proxyInput is the optional proxy step's field. It is nil for
	// providers that do not opt into the step.
	proxyInput *textinput.Model

	deviceCode      string
	userCode        string
	verificationURL string
	expiresIn       int
	interval        int
	token           *oauth.Token
}

var _ Dialog = (*OAuth)(nil)

// newOAuth creates a new device flow component.
func newOAuth(
	com *common.Common,
	isOnboarding bool,
	provider catwalk.Provider,
	model *config.SelectedModel,
	oAuthProvider OAuthProvider,
) (*OAuth, tea.Cmd) {
	t := com.Styles

	m := OAuth{}
	m.com = com
	m.isOnboarding = isOnboarding
	m.Base = NewBase(com, 60)
	m.provider = provider
	m.model = model
	m.oAuthProvider = oAuthProvider
	m.State = OAuthStateInitializing

	m.spinner = spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(t.Dialog.OAuth.Spinner),
	)

	m.help = help.New()
	m.help.Styles = t.DialogHelpStyles()

	m.keyMap.Copy = key.NewBinding(
		key.WithKeys("c"),
		key.WithHelp("c", "copy code"),
	)
	m.keyMap.CopyURL = key.NewBinding(
		key.WithKeys("u"),
		key.WithHelp("u", "copy url"),
	)
	m.keyMap.Submit = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "copy & open"),
	)
	m.keyMap.Close = CloseKey

	// A provider that needs a proxy gets to ask for one before anything
	// goes out on the network; everyone else starts authenticating now.
	if configurer, ok := oAuthProvider.(oauthProxyConfigurer); ok {
		input := textinput.New()
		input.SetVirtualCursor(false)
		input.Placeholder = "http://host:port (leave empty for none)"
		input.SetStyles(t.TextInput)
		input.SetValue(configurer.proxyURL())
		input.Focus()
		m.proxyInput = &input
		m.State = OAuthStateProxy
		return &m, nil
	}

	return &m, tea.Batch(m.spinner.Tick, m.oAuthProvider.initiateAuth)
}

// ID implements Dialog.
func (m *OAuth) ID() string {
	return OAuthID
}

// HandleMsg handles messages and state transitions.
func (m *OAuth) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		switch m.State {
		case OAuthStateInitializing, OAuthStateDisplay, OAuthStateSaving:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			if cmd != nil {
				return ActionCmd{cmd}
			}
		}

	case tea.KeyPressMsg:
		if m.State == OAuthStateProxy {
			return m.handleProxyKey(msg)
		}
		switch {
		case key.Matches(msg, m.keyMap.Copy):
			cmd := m.copyCode()
			return ActionCmd{cmd}

		case key.Matches(msg, m.keyMap.CopyURL):
			cmd := m.copyURL()
			return ActionCmd{cmd}

		case key.Matches(msg, m.keyMap.Submit):
			switch m.State {
			case OAuthStateSuccess:
				return m.confirmAndSelectModel()

			case OAuthStateSaving:
				// Save in progress; ignore submits until it finishes.
				return nil

			default:
				cmd := m.copyCodeAndOpenURL()
				return ActionCmd{cmd}
			}

		case key.Matches(msg, m.keyMap.Close):
			switch m.State {
			case OAuthStateSuccess:
				return m.confirmAndSelectModel()

			case OAuthStateSaving:
				// Save in progress; ignore submits until it finishes.
				return nil

			case OAuthStateInitializing, OAuthStateDisplay:
				// Polling may already be running (Display) or about to
				// start once ActionInitiateOAuth lands (Initializing).
				// Closing without stopping it leaks the Copilot poll loop
				// until expiresIn, and leaves the Codex loopback listener
				// bound so a second sign-in attempt can't rebind the port.
				return batchActions(ActionCmd{m.oAuthProvider.stopPolling}, ActionClose{})

			default:
				return ActionClose{}
			}
		}

	case tea.PasteMsg:
		if m.State == OAuthStateProxy && m.proxyInput != nil {
			var cmd tea.Cmd
			*m.proxyInput, cmd = m.proxyInput.Update(msg)
			if cmd != nil {
				return ActionCmd{cmd}
			}
		}

	case ActionInitiateOAuth:
		m.deviceCode = msg.DeviceCode
		m.userCode = msg.UserCode
		m.expiresIn = msg.ExpiresIn
		m.verificationURL = msg.VerificationURL
		m.interval = msg.Interval
		m.State = OAuthStateDisplay
		return ActionCmd{m.oAuthProvider.startPolling(msg.DeviceCode, msg.ExpiresIn)}

	case ActionCompleteOAuth:
		// The device flow finished and we have a token. Immediately
		// persist it and fetch models in the background (this triggers a
		// config reload that can take a few seconds), showing a spinner.
		// The success screen is presented only once that work completes,
		// so it truthfully means "ready to use" rather than gating the
		// work behind a keypress.
		m.State = OAuthStateSaving
		m.token = msg.Token
		return ActionCmd{tea.Batch(
			m.oAuthProvider.stopPolling,
			m.spinner.Tick,
			m.saveCredential(),
		)}

	case ActionOAuthErrored:
		m.State = OAuthStateError
		cmd := tea.Batch(m.oAuthProvider.stopPolling, util.ReportError(msg.Error))
		return ActionCmd{cmd}

	case oauthSaveDoneMsg:
		// Credential saved and models fetched. Present the confirmation
		// screen; the actual model selection happens when the user
		// acknowledges it (fast, since the work is already done).
		m.State = OAuthStateSuccess
		return nil

	case oauthSaveErrMsg:
		// Save failed; surface the error and move to the error state so
		// the user can dismiss and retry the flow.
		m.State = OAuthStateError
		return ActionCmd{util.ReportError(msg.err)}
	}
	return nil
}

// handleProxyKey drives the proxy step: enter accepts the value and starts
// the flow, escape dismisses the dialog, and anything else goes to the
// field. An unusable value keeps the user on the step with the reason,
// rather than failing later as an opaque sign-in error.
func (m *OAuth) handleProxyKey(msg tea.KeyPressMsg) Action {
	switch {
	case key.Matches(msg, m.keyMap.Close):
		return ActionClose{}

	case key.Matches(msg, m.keyMap.Submit):
		configurer, ok := m.oAuthProvider.(oauthProxyConfigurer)
		if !ok {
			return nil
		}
		proxy := strings.TrimSpace(m.proxyInput.Value())
		if err := configurer.setProxyURL(proxy); err != nil {
			return ActionCmd{util.ReportError(err)}
		}
		m.State = OAuthStateInitializing
		return ActionCmd{tea.Batch(m.spinner.Tick, m.oAuthProvider.initiateAuth)}

	default:
		var cmd tea.Cmd
		*m.proxyInput, cmd = m.proxyInput.Update(msg)
		if cmd != nil {
			return ActionCmd{cmd}
		}
	}
	return nil
}

// oauthProxyConfigurer is an optional half of [OAuthProvider], implemented
// by providers whose sign-in may have to go through a proxy. The dialog
// then opens on a proxy step instead of authenticating straight away.
type oauthProxyConfigurer interface {
	// proxyURL is the value to prefill, typically whatever the provider is
	// already configured with.
	proxyURL() string
	// setProxyURL applies the value the user entered, rejecting one that
	// cannot work. An empty value means no proxy.
	setProxyURL(string) error
}

// oauthSaveDoneMsg is emitted by the background save command once the
// credential has been persisted and models fetched. The model-selection
// details are read from the dialog's own fields when the user confirms.
type oauthSaveDoneMsg struct{}

// oauthSaveErrMsg is emitted by the background save command when persisting
// the credential fails.
type oauthSaveErrMsg struct {
	err error
}

// View renders the device flow dialog.
func (m *OAuth) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	m.Resize(area)

	view := m.dialogContent()
	if !m.isOnboarding {
		view = m.Frame(view)
	}

	// The proxy step is the only one with a field to type into, so it is
	// also the only one that owns a cursor. It is located in the rendered
	// view, which is why the view is built before it.
	var cur *tea.Cursor
	if m.State == OAuthStateProxy {
		cur = m.proxyCursor(view)
	}

	if m.isOnboarding {
		DrawOnboardingCursor(scr, area, view, cur)
	} else {
		DrawCenterCursor(scr, area, view, cur)
	}
	return cur
}

func (m *OAuth) dialogContent() string {
	t := m.com.Styles

	switch m.State {
	case OAuthStateInitializing, OAuthStateSaving:
		return m.innerContent()

	default:
		innerWidth := m.InnerWidth()
		elements := []string{
			m.headerContent(),
			m.innerContent(),
			renderDialogHelp(t, &m.help, m, innerWidth),
		}
		return strings.Join(elements, "\n")
	}
}

func (m *OAuth) headerContent() string {
	var (
		t            = m.com.Styles
		titleStyle   = t.Dialog.Title
		textStyle    = t.Dialog.PrimaryText
		dialogStyle  = t.Dialog.View.Width(m.Width())
		headerOffset = titleStyle.GetHorizontalFrameSize() + dialogStyle.GetHorizontalFrameSize()
		dialogTitle  = fmt.Sprintf("Let’s authenticate with %s", m.oAuthProvider.name())
	)
	if m.isOnboarding {
		return textStyle.Render(dialogTitle)
	}
	return common.DialogTitle(t, titleStyle.Render(dialogTitle), m.Width()-headerOffset, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
}

func (m *OAuth) innerContent() string {
	var (
		t                = m.com.Styles
		instructionStyle = t.Dialog.OAuth.Instructions
		enterKeyStyle    = t.Dialog.OAuth.Enter
		successStyle     = t.Dialog.OAuth.Success
		linkStyle        = t.Dialog.OAuth.Link
		errorStyle       = t.Dialog.OAuth.ErrorText
		statusTextStyle  = t.Dialog.OAuth.StatusText
	)

	// innerWidth is the dialog's content area: total width minus the
	// View frame (border). Every block sizes to this so nothing gets
	// re-wrapped when the dialog frame renders it.
	innerWidth := m.InnerWidth()

	switch m.State {
	case OAuthStateProxy:
		m.proxyInput.SetWidth(max(0, innerWidth-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
		prompt := instructionStyle.
			Width(innerWidth).
			Padding(0, 1).
			Render("Proxy for reaching " + m.oAuthProvider.name() + " (optional):")
		field := t.Dialog.InputPrompt.Render(m.proxyInput.View())
		note := statusTextStyle.
			Width(innerWidth).
			Padding(0, 1).
			Render("Leave empty to use the environment's proxy settings, or\n" +
				`type "none" to force a direct connection. It is saved with` + "\nthe provider, so model requests use it too.")

		return lipgloss.JoinVertical(
			lipgloss.Left,
			"",
			prompt,
			"",
			field,
			"",
			note,
			"",
		)

	case OAuthStateInitializing:
		return lipgloss.NewStyle().
			Width(innerWidth).
			Align(lipgloss.Center).
			Render(
				successStyle.Render(m.spinner.View()) +
					statusTextStyle.Render("Initializing..."),
			)

	case OAuthStateDisplay:
		// Render each text segment with its own style. Wrapping the
		// whole concatenation in a single style would lose the text
		// color after enterKeyStyle's reset code.
		//
		// Not every provider issues a user code: a redirect flow carries
		// everything it needs in the URL, so there is nothing to copy and
		// nothing to show in a code box.
		tail := " to copy the code below and open the browser."
		if !m.hasUserCode() {
			tail = " to open the browser and sign in."
		}
		instructionText := instructionStyle.Render("Press ") +
			enterKeyStyle.Render("enter") +
			instructionStyle.Render(tail)
		instructions := lipgloss.NewStyle().
			Width(innerWidth).
			Padding(0, 1).
			Render(instructionText)

		codeBox := ""
		if m.hasUserCode() {
			codeBox = lipgloss.NewStyle().
				Width(innerWidth).
				Height(7).
				Align(lipgloss.Center, lipgloss.Center).
				Background(t.Dialog.OAuth.UserCodeBg).
				Render(
					t.Dialog.OAuth.UserCode.Render(m.userCode),
				)
		}

		link := linkStyle.Hyperlink(m.verificationURL, "id=oauth-verify").Render(m.verificationURL)
		url := statusTextStyle.
			Width(innerWidth).
			Padding(0, 1).
			Render("Browser not opening? Pay a visit to:\n" + link)

		waiting := statusTextStyle.
			Width(innerWidth).
			Padding(0, 1).
			Render(
				successStyle.Render(m.spinner.View()) + statusTextStyle.Render("Verifying..."),
			)

		return lipgloss.JoinVertical(
			lipgloss.Left,
			"",
			instructions,
			"",
			codeBox,
			"",
			url,
			"",
			waiting,
			"",
		)

	case OAuthStateSuccess:
		return successStyle.
			Width(innerWidth).
			Padding(1).
			Render("Authentication successful!")

	case OAuthStateSaving:
		return lipgloss.NewStyle().
			Width(innerWidth).
			Align(lipgloss.Center).
			Render(
				successStyle.Render(m.spinner.View()) +
					statusTextStyle.Render(" Fetching models..."),
			)

	case OAuthStateError:
		return errorStyle.
			Width(innerWidth).
			Padding(1).
			Render("Authentication failed.")

	default:
		return ""
	}
}

// proxyCursor locates the proxy field inside the already-rendered view and
// puts the terminal cursor on it.
//
// The position is found rather than derived. The shared InputCursor helper
// encodes the API-key dialog's layout — a field directly under the title —
// and here the field sits below a header and a prompt line, inside a frame
// whose style getters under-report what it actually renders. Deriving the
// offsets from those getters is what left the cursor parked away from the
// field. Searching the rendered frame for the input's own prompt cannot
// drift from what the user sees, whatever moves above it; when the anchor
// is not found the cursor is dropped rather than guessed at.
func (m *OAuth) proxyCursor(view string) *tea.Cursor {
	if m.proxyInput == nil {
		return nil
	}
	cur := m.proxyInput.Cursor()
	if cur == nil {
		return nil
	}

	// The input reports its cursor relative to its own render, which begins
	// at the prompt — so the prompt is where the search begins too.
	anchor := ansi.Strip(m.proxyInput.Prompt + m.proxyInput.Value())
	if anchor == "" {
		return nil
	}
	for y, line := range strings.Split(view, "\n") {
		plain := ansi.Strip(line)
		x := strings.Index(plain, anchor)
		if x < 0 {
			continue
		}
		cur.X += ansi.StringWidth(plain[:x])
		cur.Y += y
		return cur
	}
	return nil
}

// FullHelp returns the full help view.
func (m *OAuth) FullHelp() [][]key.Binding {
	return [][]key.Binding{m.ShortHelp()}
}

// ShortHelp returns the full help view.
func (m *OAuth) ShortHelp() []key.Binding {
	switch m.State {
	case OAuthStateProxy:
		return []key.Binding{
			key.NewBinding(
				key.WithKeys("enter", "ctrl+y"),
				key.WithHelp("enter", "continue"),
			),
			m.keyMap.Close,
		}

	case OAuthStateError:
		return []key.Binding{m.keyMap.Close}

	case OAuthStateSuccess:
		return []key.Binding{
			key.NewBinding(
				key.WithKeys("enter", "ctrl+y", "esc"),
				key.WithHelp("enter", "finish"),
			),
		}

	case OAuthStateSaving:
		// No actionable keys while the save completes.
		return nil

	default:
		binds := []key.Binding{}
		if m.hasUserCode() {
			binds = append(binds, m.keyMap.Copy)
		}
		return append(binds,
			m.keyMap.CopyURL,
			m.keyMap.Submit,
			m.keyMap.Close,
		)
	}
}

// hasUserCode reports whether this flow shows the user a code to type into
// the browser (a device flow) rather than sending them to a URL that
// already carries the authorization request (a redirect flow).
func (m *OAuth) hasUserCode() bool {
	return m.userCode != ""
}

func (m *OAuth) copyCode() tea.Cmd {
	if m.State != OAuthStateDisplay || !m.hasUserCode() {
		return nil
	}
	return common.CopyToClipboard(m.userCode, "Code copied to clipboard")
}

func (m *OAuth) copyURL() tea.Cmd {
	if m.State != OAuthStateDisplay {
		return nil
	}
	return common.CopyToClipboard(m.verificationURL, "URL copied to clipboard")
}

func (m *OAuth) copyCodeAndOpenURL() tea.Cmd {
	if m.State != OAuthStateDisplay {
		return nil
	}
	if !m.hasUserCode() {
		// Nothing to copy: the URL is the whole authorization request.
		return func() tea.Msg {
			if err := browser.OpenURL(m.verificationURL); err != nil {
				return ActionOAuthErrored{fmt.Errorf("failed to open browser: %w", err)}
			}
			return nil
		}
	}
	return common.CopyToClipboardWithCallback(
		m.userCode,
		"Code copied and URL opened",
		func() tea.Msg {
			if err := browser.OpenURL(m.verificationURL); err != nil {
				return ActionOAuthErrored{fmt.Errorf("failed to open browser: %w", err)}
			}
			return nil
		},
	)
}

// saveCredential returns a command that persists the OAuth token and
// triggers the config reload (including model discovery) off the UI update
// loop. It reports completion via oauthSaveDoneMsg or oauthSaveErrMsg.
func (m *OAuth) saveCredential() tea.Cmd {
	// Capture the fields the command needs so it does not race with
	// dialog state.
	var (
		com      = m.com
		provider = m.provider
		token    = m.token
	)
	oAuthProvider := m.oAuthProvider
	return func() tea.Msg {
		if err := com.Workspace.SetProviderAPIKey(config.ScopeGlobal, string(provider.ID), token); err != nil {
			return oauthSaveErrMsg{err: fmt.Errorf("failed to save API key: %w", err)}
		}
		// Some providers have more to store than the credential itself —
		// Codex, whose model list is per-account and only readable once
		// there is a token to read it with.
		if saver, ok := oAuthProvider.(oauthPostSaver); ok {
			if err := saver.afterSave(com.Workspace, token); err != nil {
				return oauthSaveErrMsg{err: err}
			}
		}
		return oauthSaveDoneMsg{}
	}
}

// oauthPostSaver is the optional half of [OAuthProvider], implemented by
// providers that need to write more than the token once it is saved.
type oauthPostSaver interface {
	afterSave(ws workspace.ConfigAccessor, token *oauth.Token) error
}

// confirmAndSelectModel is invoked when the user acknowledges the success
// screen. The credential is already saved, so this only resumes model
// selection, which closes the dialog.
func (m *OAuth) confirmAndSelectModel() Action {
	if m.model == nil {
		return ActionProviderConfigured{ProviderID: string(m.provider.ID)}
	}
	return ActionSelectModel{
		Provider: m.provider,
		Model:    *m.model,
	}
}
