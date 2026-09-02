package dialog

import (
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// textEndColumn is the screen column just past needle in line, counted in
// display cells rather than bytes — the dialog frame draws box characters
// that are several bytes wide but one column each.
func textEndColumn(line, needle string) int {
	idx := strings.Index(line, needle)
	if idx < 0 {
		return -1
	}
	return lipgloss.Width(line[:idx]) + lipgloss.Width(needle)
}

// newCodexDialogWith builds the dialog over ws, which stands in for the
// backend the sign-in now lives behind (see workspace.OAuthController).
func newCodexDialogWith(t *testing.T, ws *completeOAuthTestWorkspace) *OAuth {
	t.Helper()
	s := styles.SennitDark()
	com := &common.Common{Styles: &s, Workspace: ws}
	provider := catwalk.Provider{ID: catwalk.InferenceProvider(CodexProviderID), Name: codexProviderName}
	dlg, cmd := NewOAuthCodex(com, false, provider, nil, false)
	// The proxy step prefills asynchronously (see oauthProxyPrefillMsg) so
	// the constructor never touches disk itself; run that cmd here to
	// reach the same state the real event loop would.
	if cmd != nil {
		if msg := cmd(); msg != nil {
			dlg.HandleMsg(msg)
		}
	}
	return dlg
}

func newCodexDialog(t *testing.T) *OAuth {
	t.Helper()
	return newCodexDialogWith(t, &completeOAuthTestWorkspace{})
}

// TestOAuthCodexStartsOnProxyStep: Codex is unreachable without a proxy for
// some users, so the dialog must ask before it touches the network — a flow
// started first would simply fail.
func TestOAuthCodexStartsOnProxyStep(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	require.Equal(t, OAuthStateProxy, dlg.State)
	require.NotNil(t, dlg.proxyInput)
}

// TestOAuthCopilotSkipsProxyStep keeps the step opt-in: a device flow that
// never needed one must not grow an extra screen.
func TestOAuthCopilotSkipsProxyStep(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
	dlg, _ := NewOAuthCopilot(com, false, provider, nil, false)

	require.Equal(t, OAuthStateInitializing, dlg.State)
	require.Nil(t, dlg.proxyInput)
}

// TestOAuthCodexRejectsBadProxy: an unusable value keeps the user on the
// step, rather than starting a sign-in that fails a few seconds later with
// something less obviously about the proxy.
func TestOAuthCodexRejectsBadProxy(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	dlg.proxyInput.SetValue("ftp://nope:21")

	action := dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	require.Equal(t, OAuthStateProxy, dlg.State, "a bad proxy must not start the flow")
	require.IsType(t, ActionCmd{}, action, "the failure must be reported to the user")
}

// TestOAuthCodexAcceptsProxy: a good value is handed to the provider and
// moves the dialog on to authenticating.
func TestOAuthCodexAcceptsProxy(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	dlg.proxyInput.SetValue("  socks5://127.0.0.1:1080  ")

	dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	require.Equal(t, OAuthStateInitializing, dlg.State)
	provider, ok := dlg.oAuthProvider.(*OAuthCodex)
	require.True(t, ok)
	require.Equal(t, "socks5://127.0.0.1:1080", provider.proxy, "surrounding whitespace must be trimmed")
}

// TestOAuthCodexEmptyProxyIsAllowed: the setting is optional, and an empty
// field means "whatever the environment does".
func TestOAuthCodexEmptyProxyIsAllowed(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)

	dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	require.Equal(t, OAuthStateInitializing, dlg.State)
	provider, ok := dlg.oAuthProvider.(*OAuthCodex)
	require.True(t, ok)
	require.Empty(t, provider.proxy)
}

// TestOAuthCodexProxyStepTypes: keys that are not enter or escape edit the
// field instead of falling through to the display-state bindings, where "c"
// and "u" are copy shortcuts.
func TestOAuthCodexProxyStepTypes(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	for _, r := range "custom" {
		dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}

	require.Equal(t, "custom", dlg.proxyInput.Value())
	require.Equal(t, OAuthStateProxy, dlg.State)
}

// TestOAuthCodexPrefillsConfiguredProxy: the value the provider already
// uses — Sennit's own configuration, or the Codex CLI's on this machine,
// as the backend resolves it (see AppWorkspace.OAuthConfiguredProxy and
// its own tests) — is offered back rather than asked for again.
func TestOAuthCodexPrefillsConfiguredProxy(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialogWith(t, &completeOAuthTestWorkspace{configuredProxy: "http://127.0.0.1:8080"})
	require.Equal(t, "http://127.0.0.1:8080", dlg.proxyInput.Value())
}

// TestNewOAuthCodex_DoesNotReadDiskInConstructor is the regression test for
// the proxy step's prefill: newOAuth used to call configurer.proxyURL()
// (a config file read, which for Codex reaches the CLI's own config on
// disk) directly in the constructor, before it ever returned a tea.Cmd. It
// must instead defer that read to the returned cmd, landing as
// oauthProxyPrefillMsg once it completes, so opening the dialog never
// blocks Update on IO.
func TestNewOAuthCodex_DoesNotReadDiskInConstructor(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	ws := &completeOAuthTestWorkspace{configuredProxy: "http://127.0.0.1:9090"}
	com := &common.Common{Styles: &s, Workspace: ws}
	provider := catwalk.Provider{ID: catwalk.InferenceProvider(CodexProviderID), Name: codexProviderName}

	dlg, cmd := NewOAuthCodex(com, false, provider, nil, false)
	require.NotNil(t, cmd, "the read must be deferred to a tea.Cmd")
	require.Equal(t, OAuthStateProxy, dlg.State)
	require.Empty(t, dlg.proxyInput.Value(),
		"the constructor must not have read the configured proxy before returning")

	msg := cmd()
	prefill, ok := msg.(oauthProxyPrefillMsg)
	require.True(t, ok, "expected oauthProxyPrefillMsg, got %T", msg)
	require.Equal(t, "http://127.0.0.1:9090", prefill.value)

	dlg.HandleMsg(prefill)
	require.Equal(t, "http://127.0.0.1:9090", dlg.proxyInput.Value())
}

// TestOAuthCodexCursorSitsOnProxyField draws the step and checks the cursor
// lands on the input's own line, right after what has been typed. The
// offsets are the easy thing to get wrong — the shared InputCursor helper
// assumes a layout where the field follows the title directly, and here it
// does not — and the symptom is a cursor parked somewhere meaningless.
func TestOAuthCodexCursorSitsOnProxyField(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	for _, r := range "zzproxyzz" {
		dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}

	area := image.Rect(0, 0, 100, 40)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	cur := dlg.Draw(scr, area)
	require.NotNil(t, cur, "the proxy step must show a cursor to type at")

	lines := strings.Split(scr.String(), "\n")
	require.Less(t, cur.Y, len(lines))

	line := lines[cur.Y]
	require.Contains(t, line, "zzproxyzz",
		"cursor row %d must be the proxy field's line, got %q", cur.Y, line)

	// The cursor belongs just past the typed text, not at the start of the
	// line or off in the padding beyond it. Columns, not bytes: the frame's
	// box-drawing characters are multi-byte.
	require.Equal(t, textEndColumn(line, "zzproxyzz"), cur.X,
		"cursor must sit right after the typed value on %q", line)
}

// TestOAuthCodexCursorDuringOnboarding: onboarding draws the step without
// the dialog frame, and the cursor has to follow it there too.
func TestOAuthCodexCursorDuringOnboarding(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	provider := catwalk.Provider{ID: catwalk.InferenceProvider(CodexProviderID), Name: codexProviderName}
	dlg, _ := NewOAuthCodex(com, true, provider, nil, false)
	for _, r := range "zzproxyzz" {
		dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}

	area := image.Rect(0, 0, 100, 40)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	cur := dlg.Draw(scr, area)
	require.NotNil(t, cur)

	lines := strings.Split(scr.String(), "\n")
	require.Less(t, cur.Y, len(lines))
	line := lines[cur.Y]
	require.Contains(t, line, "zzproxyzz",
		"cursor row %d must be the proxy field's line, got %q", cur.Y, line)
	require.Equal(t, textEndColumn(line, "zzproxyzz"), cur.X,
		"cursor must sit right after the typed value on %q", line)
}

// TestOAuthCodexStartPollingSetsCancelFuncBeforeReturning: startPolling used
// to assign m.cancelFunc inside the returned closure, so it was both set on
// the cmd goroutine (racing Update) and possibly not set yet if the user hit
// esc before the closure ran, leaving stopPolling with nothing to cancel.
// The fix assigns it in startPolling's body, mirroring OAuthCopilot.
func TestOAuthCodexStartPollingSetsCancelFuncBeforeReturning(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	provider, ok := dlg.oAuthProvider.(*OAuthCodex)
	require.True(t, ok)

	flow := &stubDialogOAuthFlow{}
	t.Cleanup(flow.Cancel)
	provider.flow = flow

	cmd := provider.startPolling("", 0)
	require.NotNil(t, cmd)
	require.NotNil(t, provider.cancelFunc,
		"cancelFunc must be set before the closure runs, not inside it")

	// stopPolling (esc) must be able to cancel even if the closure returned
	// by startPolling never gets scheduled.
	provider.stopPolling()
	require.Nil(t, provider.cancelFunc)
}

// TestOAuthCodexHidesCursorAfterProxyStep: once the step is done there is
// nothing to type into, so a cursor left behind would point at nothing.
func TestOAuthCodexHidesCursorAfterProxyStep(t *testing.T) {
	t.Parallel()

	dlg := newCodexDialog(t)
	dlg.State = OAuthStateDisplay
	dlg.verificationURL = "https://auth.openai.com/oauth/authorize"

	area := image.Rect(0, 0, 100, 40)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.Nil(t, dlg.Draw(scr, area))
}

// TestOAuthCodexInitiateAuthShortCircuitsOnExistingLogin: when the backend
// reports a token it could reuse or refresh from an existing Codex CLI
// login, there is nothing to show the user — the dialog goes straight to
// saving.
func TestOAuthCodexInitiateAuthShortCircuitsOnExistingLogin(t *testing.T) {
	t.Parallel()

	token := &oauth.Token{AccessToken: "from-disk"}
	ws := &completeOAuthTestWorkspace{
		startResult: workspace.OAuthStartResult{Token: token, ReusedExistingLogin: true},
	}
	dlg := newCodexDialogWith(t, ws)
	provider, ok := dlg.oAuthProvider.(*OAuthCodex)
	require.True(t, ok)

	msg := provider.initiateAuth()
	complete, ok := msg.(ActionCompleteOAuth)
	require.True(t, ok, "expected ActionCompleteOAuth, got %#v", msg)
	require.Equal(t, token, complete.Token)
	require.Nil(t, provider.flow, "a short-circuited sign-in has no flow to wait on")
}

// TestOAuthCodexInitiateAuthCancelsFlowOnceStopped covers the esc-during-
// Initializing race: the flow binds a fixed callback port, so one that
// lands after the dialog is gone must be released rather than left bound
// for the next sign-in to fail on.
func TestOAuthCodexInitiateAuthCancelsFlowOnceStopped(t *testing.T) {
	t.Parallel()

	flow := &stubDialogOAuthFlow{}
	ws := &completeOAuthTestWorkspace{
		startResult: workspace.OAuthStartResult{AuthorizationURL: "https://auth.openai.com/oauth/authorize"},
		startFlow:   flow,
	}
	dlg := newCodexDialogWith(t, ws)
	provider, ok := dlg.oAuthProvider.(*OAuthCodex)
	require.True(t, ok)

	provider.stopPolling()
	require.Nil(t, provider.initiateAuth(), "a dismissed dialog's flow result must not surface")
	require.Equal(t, 1, flow.cancelCount(), "the late flow must be released")
}

// TestOAuthCodexInitiateAuthReportsURL is the ordinary path: the backend's
// authorization URL is what the display state shows.
func TestOAuthCodexInitiateAuthReportsURL(t *testing.T) {
	t.Parallel()

	flow := &stubDialogOAuthFlow{}
	ws := &completeOAuthTestWorkspace{
		startResult: workspace.OAuthStartResult{AuthorizationURL: "https://auth.openai.com/oauth/authorize?x=1"},
		startFlow:   flow,
	}
	dlg := newCodexDialogWith(t, ws)
	provider, ok := dlg.oAuthProvider.(*OAuthCodex)
	require.True(t, ok)

	msg := provider.initiateAuth()
	initiate, ok := msg.(ActionInitiateOAuth)
	require.True(t, ok, "expected ActionInitiateOAuth, got %#v", msg)
	require.Equal(t, "https://auth.openai.com/oauth/authorize?x=1", initiate.VerificationURL)
	require.Empty(t, initiate.UserCode, "a redirect flow has no code to type")

	provider.stopPolling()
	require.Equal(t, 1, flow.cancelCount(), "closing the dialog must release the callback port")
}
