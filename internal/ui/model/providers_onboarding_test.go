package model

import (
	"context"
	"reflect"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/catwalk/pkg/catwalk"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// runTeaCmd executes cmd (and, recursively, every sub-command it produces)
// the way the Bubble Tea runtime would when it encounters a tea.Batch or
// tea.Sequence result. Both wrap their sub-commands in an unexported slice
// type, so this walks the result via reflection instead of a type switch on
// tea.BatchMsg/tea.Sequence's internal message type — that lets the
// initAgentAndReportModel tail (InitCoderAgent/UpdateAgentModel run inside
// tea.Sequence's first slot) actually execute during a test.
func runTeaCmd(cmd tea.Cmd) {
	collectTeaMsgs(cmd)
}

// collectTeaMsgs is runTeaCmd's sibling: it walks the same tea.Batch/
// tea.Sequence tree but also gathers every leaf (non-slice) message so
// tests can assert on what the command chain actually reported.
func collectTeaMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for i := range v.Len() {
		if sub, ok := v.Index(i).Interface().(tea.Cmd); ok {
			out = append(out, collectTeaMsgs(sub)...)
		}
	}
	return out
}

// preferredModelCall records one UpdatePreferredModel invocation so tests
// can assert what model landed.
type preferredModelCall struct {
	scope config.Scope
	model config.SelectedModel
}

// onboardingTestWorkspace is a [workspace.Workspace] stub covering the
// onboarding-provider-selection flow: New()/Init() (which probe agent
// readiness and project state), plus ActionProviderConfigured's model
// bootstrap (UpdatePreferredModel, InitCoderAgent, UpdateAgentModel). It
// must embed the full interface (see testWorkspace above for the
// rationale).
type onboardingTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config

	preferredCalls []preferredModelCall

	initCoderAgentCalled   bool
	updateAgentModelCalled bool
}

func (w *onboardingTestWorkspace) Config() *config.Config { return w.cfg }

func (w *onboardingTestWorkspace) PermissionSkipRequests() bool { return false }

func (w *onboardingTestWorkspace) AgentIsReady() bool { return false }

func (w *onboardingTestWorkspace) AgentModel() workspace.AgentModel { return workspace.AgentModel{} }

func (w *onboardingTestWorkspace) ProjectNeedsInitialization() (bool, error) { return false, nil }

func (w *onboardingTestWorkspace) UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error {
	w.preferredCalls = append(w.preferredCalls, preferredModelCall{scope, model})
	return nil
}

func (w *onboardingTestWorkspace) InitCoderAgent(_ context.Context) error {
	w.initCoderAgentCalled = true
	return nil
}

func (w *onboardingTestWorkspace) UpdateAgentModel(_ context.Context) error {
	w.updateAgentModelCalled = true
	return nil
}

// stubActionDialog is a minimal [dialog.Dialog] that returns a fixed Action
// for any message, letting tests drive handleDialogMsg's switch without
// building a real dialog's internal state.
type stubActionDialog struct {
	id     string
	action dialog.Action
}

func (d *stubActionDialog) ID() string { return d.id }

func (d *stubActionDialog) HandleMsg(tea.Msg) dialog.Action { return d.action }

func (d *stubActionDialog) Draw(uv.Screen, uv.Rectangle) *tea.Cursor { return nil }

// TestNew_UnconfiguredEntersOnboardingAndOpensProvidersDialog pins the
// onboarding entry point: with no providers configured, New() must select
// uiOnboarding, and Init() must open the Providers dialog (not the old
// Models dialog).
func TestNew_UnconfiguredEntersOnboardingAndOpensProvidersDialog(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{TUI: &config.TUIOptions{}},
	}
	ws := &onboardingTestWorkspace{cfg: cfg}
	com := common.DefaultCommon(context.Background(), ws)

	ui := New(com, "", false)
	require.Equal(t, uiOnboarding, ui.state)

	ui.Init()
	require.True(t, ui.dialog.ContainsDialog(dialog.ProvidersID),
		"Init must open the Providers dialog during onboarding")
	require.False(t, ui.dialog.ContainsDialog(dialog.ModelsID),
		"onboarding must no longer open the Models dialog directly")
}

// newOnboardingTestUI builds a *UI with just enough state for
// handleDialogMsg's ActionProviderConfigured case, without going through
// the heavier New(). A stub dialog that always returns the given action is
// pushed onto the overlay so handleDialogMsg's call to m.dialog.Update
// surfaces it, mirroring how a real dialog's HandleMsg would. It also wires
// up status/chat/editor/keyMap like newBusyUI in session_busy_test.go,
// since the onboarding branch calls setState, which reruns layout and
// touches those fields.
func newOnboardingTestUI(ws *onboardingTestWorkspace, state uiState, action dialog.Action) *UI {
	com := common.DefaultCommon(context.Background(), ws)
	overlay := dialog.NewOverlay(&stubActionDialog{id: "stub", action: action})
	ui := &UI{
		com:    com,
		chat:   NewChat(com, config.ScrollbarDefault),
		editor: editorState{textarea: textarea.New()},
		state:  state,
		focus:  uiFocusEditor,
		width:  140,
		height: 45,
		keyMap: DefaultKeyMap(),
		dialog: overlay,
	}
	ui.status = NewStatus(com, ui)
	return ui
}

// newProviderConfiguredTestConfig builds a *config.Config with providerID
// already configured (simulating "a provider was just configured") and no
// model selection yet (simulating first run), plus a single catalog model
// for providerID so DefaultModelForProvider has something to fall back to.
func newProviderConfiguredTestConfig(providerID string) *config.Config {
	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{TUI: &config.TUIOptions{}},
	}
	cfg.Providers.Set(providerID, config.ProviderConfig{
		ID:     providerID,
		Models: []catwalk.Model{{ID: "default-model"}},
	})
	return cfg
}

// TestActionProviderConfigured_OnboardingSelectsDefaultModelAndEntersLanding
// covers the first-run path: a provider was just configured, no model has
// ever been selected, so the handler must fall back to the provider's own
// default model, persist it as the configured model, bring the coder agent
// up, and transition into uiLanding.
func TestActionProviderConfigured_OnboardingSelectsDefaultModelAndEntersLanding(t *testing.T) {
	t.Parallel()

	const providerID = "custom-test-provider"

	cfg := newProviderConfiguredTestConfig(providerID)
	ws := &onboardingTestWorkspace{cfg: cfg}
	action := dialog.ActionProviderConfigured{ProviderID: providerID}
	ui := newOnboardingTestUI(ws, uiOnboarding, action)

	cmd := ui.handleDialogMsg(struct{}{})
	require.NotNil(t, cmd)
	runTeaCmd(cmd)

	require.Equal(t, uiLanding, ui.state)
	require.False(t, ui.dialog.ContainsDialog(dialog.ProvidersID))

	require.Len(t, ws.preferredCalls, 1, "expected the model to be persisted once")
	call := ws.preferredCalls[0]
	require.Equal(t, config.ScopeGlobal, call.scope)
	require.Equal(t, providerID, call.model.Provider)
	require.Equal(t, "default-model", call.model.Model,
		"first run must fall back to the provider's own default model")

	require.True(t, ws.initCoderAgentCalled)
	require.True(t, ws.updateAgentModelCalled)
}

// TestActionProviderConfigured_ReportsModelChangedStatus covers the status
// toast emitted once onboarding lands a model: it must read "Model changed
// to ...", not the old "Large model changed to ..." wording — there is only
// one configurable model now, so the type label is gone entirely.
func TestActionProviderConfigured_ReportsModelChangedStatus(t *testing.T) {
	t.Parallel()

	const providerID = "custom-test-provider"

	cfg := newProviderConfiguredTestConfig(providerID)
	ws := &onboardingTestWorkspace{cfg: cfg}
	action := dialog.ActionProviderConfigured{ProviderID: providerID}
	ui := newOnboardingTestUI(ws, uiOnboarding, action)

	cmd := ui.handleDialogMsg(struct{}{})
	require.NotNil(t, cmd)
	msgs := collectTeaMsgs(cmd)

	var infoMsgs []string
	for _, msg := range msgs {
		if info, ok := msg.(util.InfoMsg); ok {
			infoMsgs = append(infoMsgs, info.Msg)
		}
	}
	require.Contains(t, infoMsgs, "Model changed to default-model")
}

// TestActionProviderConfigured_OutsideOnboardingOnlyClosesDialogs covers
// the ordinary (non-onboarding) "configure a provider from the command
// palette" flow: the dialogs close, but nothing about the current model
// selection changes.
func TestActionProviderConfigured_OutsideOnboardingOnlyClosesDialogs(t *testing.T) {
	t.Parallel()

	const providerID = "custom-test-provider"

	cfg := newProviderConfiguredTestConfig(providerID)
	ws := &onboardingTestWorkspace{cfg: cfg}
	action := dialog.ActionProviderConfigured{ProviderID: providerID}
	ui := newOnboardingTestUI(ws, uiChat, action)

	cmd := ui.handleDialogMsg(struct{}{})
	runTeaCmd(cmd)

	require.Equal(t, uiChat, ui.state)
	require.False(t, ui.dialog.ContainsDialog(dialog.ProvidersID))
	require.Empty(t, ws.preferredCalls)
	require.False(t, ws.initCoderAgentCalled)
	require.False(t, ws.updateAgentModelCalled)
}
