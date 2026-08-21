package dialog

import (
	"errors"
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// apiKeyTestWorkspace is a minimal [workspace.Workspace] stub: it must
// embed the full interface (see testWorkspace in internal/ui/model/ui_test.go
// for the rationale) even though these tests only exercise
// SetProviderAPIKey.
type apiKeyTestWorkspace struct {
	workspace.Workspace
	savedProviderID string
	savedAPIKey     string
	saveErr         error

	// setCalls counts SetProviderAPIKey invocations, mirroring
	// countingWorkspace.syncProbes() in internal/ui/model/session_busy_test.go:
	// saveAPIKeyCmd must only ever be reached through the [tea.Cmd] HandleMsg
	// returns, never synchronously from HandleMsg itself.
	setCalls int
}

func (w *apiKeyTestWorkspace) SupportsThreads() bool { return false }

func (w *apiKeyTestWorkspace) SetProviderAPIKey(_ config.Scope, providerID string, apiKey any) error {
	w.setCalls++
	if w.saveErr != nil {
		return w.saveErr
	}
	w.savedProviderID = providerID
	w.savedAPIKey = apiKey.(string)
	return nil
}

func newAPIKeyTestCommon(t *testing.T) (*common.Common, *apiKeyTestWorkspace) {
	t.Helper()
	s := styles.SennitDark()
	ws := &apiKeyTestWorkspace{}
	return &common.Common{Styles: &s, Workspace: ws}, ws
}

// TestAPIKeyInput_DrawFramedFromConstructor guards constructor initialization
// required by the shared framed-dialog layout.
func TestAPIKeyInput_DrawFramedFromConstructor(t *testing.T) {
	com, _ := newAPIKeyTestCommon(t)
	provider := catwalk.Provider{ID: catwalk.InferenceProvider("test-provider"), Name: "Test Provider"}
	dlg, _ := NewAPIKeyInput(com, false, provider, nil)
	area := image.Rect(0, 0, 80, 24)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())

	var cursor *tea.Cursor
	require.NotPanics(t, func() {
		cursor = dlg.Draw(scr, area)
	})

	require.Equal(t, 60, dlg.Width())
	require.Positive(t, dlg.InnerWidth())
	require.NotNil(t, cursor)
	require.Contains(t, scr.String(), "Test Provider Key")
	require.Contains(t, scr.String(), "Enter your API key")
	require.Contains(t, scr.String(), "global configuration")
	require.True(t, strings.ContainsAny(scr.String(), "┌╭╔"), "framed mode should render a dialog border")
	require.GreaterOrEqual(t, cursor.X, area.Min.X)
	require.Less(t, cursor.X, area.Max.X)
	require.GreaterOrEqual(t, cursor.Y, area.Min.Y)
	require.Less(t, cursor.Y, area.Max.Y)
}

// TestAPIKeyInput_WithModelReturnsActionSelectModel is a regression test
// for the pre-existing model-switch/onboarding flow: with a non-nil model,
// submitting a verified key must still produce ActionSelectModel. The save
// itself happens off the Update goroutine (see saveAPIKeyCmd), so the test
// drives it the way the real program loop would: HandleMsg returns an
// ActionCmd, running that Cmd performs the save and yields
// ActionAPIKeySaved, and feeding that back into HandleMsg produces the
// final action — skip past the network-calling verifyAPIKey by injecting
// the "verified" state directly, then press the submit key.
func TestAPIKeyInput_WithModelReturnsActionSelectModel(t *testing.T) {
	com, ws := newAPIKeyTestCommon(t)

	provider := catwalk.Provider{ID: catwalk.InferenceProvider("test-provider"), Name: "Test Provider"}
	model := config.SelectedModel{Provider: "test-provider", Model: "test-model"}
	dlg, _ := NewAPIKeyInput(com, false, provider, &model)

	dlg.HandleMsg(ActionChangeAPIKeyState{State: APIKeyInputStateVerified})
	for _, r := range "sk-test-key" {
		dlg.HandleMsg(keyMsg(r))
	}

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected ActionCmd carrying the async save, got %#v", action)
	require.Zero(t, ws.setCalls, "HandleMsg must not call SetProviderAPIKey synchronously")

	saved := cmdAction.Cmd()
	action = dlg.HandleMsg(saved)
	selectAction, ok := action.(ActionSelectModel)
	require.True(t, ok, "expected ActionSelectModel, got %#v", action)
	require.Equal(t, provider, selectAction.Provider)
	require.Equal(t, model, selectAction.Model)
	require.Equal(t, 1, ws.setCalls)
	require.Equal(t, "test-provider", ws.savedProviderID)
	require.Equal(t, "sk-test-key", ws.savedAPIKey)
}

// TestAPIKeyInput_WithoutModelReturnsActionProviderConfigured covers the
// new model-less mode (used by the providers-configuration dialog): with a
// nil model, submitting a verified key produces ActionProviderConfigured
// instead of ActionSelectModel, once the async save (see
// TestAPIKeyInput_WithModelReturnsActionSelectModel) completes.
func TestAPIKeyInput_WithoutModelReturnsActionProviderConfigured(t *testing.T) {
	com, ws := newAPIKeyTestCommon(t)

	provider := catwalk.Provider{ID: catwalk.InferenceProvider("test-provider"), Name: "Test Provider"}
	dlg, _ := NewAPIKeyInput(com, false, provider, nil)

	dlg.HandleMsg(ActionChangeAPIKeyState{State: APIKeyInputStateVerified})
	for _, r := range "sk-test-key" {
		dlg.HandleMsg(keyMsg(r))
	}

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected ActionCmd carrying the async save, got %#v", action)

	saved := cmdAction.Cmd()
	action = dlg.HandleMsg(saved)
	configuredAction, ok := action.(ActionProviderConfigured)
	require.True(t, ok, "expected ActionProviderConfigured, got %#v", action)
	require.Equal(t, "test-provider", configuredAction.ProviderID)
	require.Equal(t, "test-provider", ws.savedProviderID)
	require.Equal(t, "sk-test-key", ws.savedAPIKey)
}

// TestAPIKeyInput_SaveErrorReportsAndKeepsDialogOpen pins the error path:
// a failed save must not silently drop the failure, and must not produce a
// follow-up action that would close the dialog or advance the flow.
func TestAPIKeyInput_SaveErrorReportsAndKeepsDialogOpen(t *testing.T) {
	com, ws := newAPIKeyTestCommon(t)
	ws.saveErr = errors.New("disk full")

	provider := catwalk.Provider{ID: catwalk.InferenceProvider("test-provider"), Name: "Test Provider"}
	dlg, _ := NewAPIKeyInput(com, false, provider, nil)

	dlg.HandleMsg(ActionChangeAPIKeyState{State: APIKeyInputStateVerified})
	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected ActionCmd carrying the async save, got %#v", action)

	saved := cmdAction.Cmd()
	action = dlg.HandleMsg(saved)
	reportAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected the error to be reported via ActionCmd, got %#v", action)
	require.NotNil(t, reportAction.Cmd)
}
