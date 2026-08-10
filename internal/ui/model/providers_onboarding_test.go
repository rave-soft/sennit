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
	if cmd == nil {
		return
	}
	msg := cmd()
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice {
		return
	}
	for i := range v.Len() {
		if sub, ok := v.Index(i).Interface().(tea.Cmd); ok {
			runTeaCmd(sub)
		}
	}
}

// preferredModelCall records one UpdatePreferredModel invocation so tests
// can assert both which model type was persisted and what model landed.
type preferredModelCall struct {
	scope     config.Scope
	modelType config.SelectedModelType
	model     config.SelectedModel
}

// onboardingTestWorkspace is a [workspace.Workspace] stub covering the
// onboarding-provider-selection flow: New()/Init() (which probe agent
// readiness and project state), plus ActionProviderConfigured's model
// bootstrap (UpdatePreferredModel, GetDefaultSmallModel, InitCoderAgent,
// UpdateAgentModel). It must embed the full interface (see testWorkspace
// above for the rationale).
type onboardingTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config

	preferredCalls []preferredModelCall
	smallModel     config.SelectedModel

	initCoderAgentCalled   bool
	updateAgentModelCalled bool
}

func (w *onboardingTestWorkspace) Config() *config.Config { return w.cfg }

func (w *onboardingTestWorkspace) PermissionSkipRequests() bool { return false }

func (w *onboardingTestWorkspace) AgentIsReady() bool { return false }

func (w *onboardingTestWorkspace) AgentModel() workspace.AgentModel { return workspace.AgentModel{} }

func (w *onboardingTestWorkspace) ProjectNeedsInitialization() (bool, error) { return false, nil }

func (w *onboardingTestWorkspace) UpdatePreferredModel(scope config.Scope, modelType config.SelectedModelType, model config.SelectedModel) error {
	w.preferredCalls = append(w.preferredCalls, preferredModelCall{scope, modelType, model})
	return nil
}

func (w *onboardingTestWorkspace) GetDefaultSmallModel(_ string) config.SelectedModel {
	return w.smallModel
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
// default model, persist it as both the large and small model, bring the
// coder agent up, and transition into uiLanding.
func TestActionProviderConfigured_OnboardingSelectsDefaultModelAndEntersLanding(t *testing.T) {
	t.Parallel()

	const providerID = "custom-test-provider"

	cfg := newProviderConfiguredTestConfig(providerID)
	ws := &onboardingTestWorkspace{
		cfg:        cfg,
		smallModel: config.SelectedModel{Provider: providerID, Model: "small-model"},
	}
	action := dialog.ActionProviderConfigured{ProviderID: providerID}
	ui := newOnboardingTestUI(ws, uiOnboarding, action)

	cmd := ui.handleDialogMsg(struct{}{})
	require.NotNil(t, cmd)
	runTeaCmd(cmd)

	require.Equal(t, uiLanding, ui.state)
	require.False(t, ui.dialog.ContainsDialog(dialog.ProvidersID))

	require.Len(t, ws.preferredCalls, 2, "expected the large and small model to be persisted")
	large := ws.preferredCalls[0]
	require.Equal(t, config.ScopeGlobal, large.scope)
	require.Equal(t, config.SelectedModelTypeLarge, large.modelType)
	require.Equal(t, providerID, large.model.Provider)
	require.Equal(t, "default-model", large.model.Model,
		"first run must fall back to the provider's own default model")

	small := ws.preferredCalls[1]
	require.Equal(t, config.SelectedModelTypeSmall, small.modelType)
	require.Equal(t, ws.smallModel, small.model)

	require.True(t, ws.initCoderAgentCalled)
	require.True(t, ws.updateAgentModelCalled)
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
