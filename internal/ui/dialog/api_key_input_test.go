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

func (w *apiKeyTestWorkspace) Resolver() config.VariableResolver { return config.IdentityResolver() }

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

// TestAPIKeyInput_VerifySnapshotsInputBeforeAsyncCheck is a regression test
// for verifyAPIKeyCmd reading model state off the Update goroutine. The old
// code (m.verifyAPIKey used directly as a tea.Cmd) called m.input.Value()
// lazily, when bubbletea invoked the returned func on its own goroutine —
// so any HandleMsg call reached before that goroutine ran (a paste, say)
// raced with it and could change which key got verified. The fix snapshots
// the key inside HandleMsg, before the cmd is returned.
//
// The Alibaba-Singapore provider is used because TestConnection checks that
// provider's key with a plain prefix check and returns before doing any
// network I/O (see (*config.ProviderConfig).TestConnection), which keeps
// this test hermetic while still exercising the real snapshot/lazy-read
// difference.
func TestAPIKeyInput_VerifySnapshotsInputBeforeAsyncCheck(t *testing.T) {
	com, _ := newAPIKeyTestCommon(t)
	provider := catwalk.Provider{ID: catwalk.InferenceProviderAlibabaSingapore, Name: "Alibaba"}
	dlg, _ := NewAPIKeyInput(com, false, provider, nil)

	for _, r := range "sk-original" {
		dlg.HandleMsg(keyMsg(r))
	}
	dlg.input.CursorStart()

	action := dlg.HandleMsg(ActionChangeAPIKeyState{State: APIKeyInputStateVerifying})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "expected ActionCmd carrying the batched verify, got %#v", action)

	// Simulate a message reaching HandleMsg after the Verifying transition
	// but before the async cmd runs — the window the old lazy read raced
	// against. Pasting at the (rewound) start of the field breaks the
	// "sk-" prefix check, so this only passes if the cmd already captured
	// "sk-original" rather than reading m.input lazily when it runs.
	dlg.HandleMsg(tea.PasteMsg{Content: "nope"})

	batch, ok := cmdAction.Cmd().(tea.BatchMsg)
	require.True(t, ok, "expected a batched command (spinner tick + verify)")

	var gotResult bool
	for _, sub := range batch {
		msg := sub()
		if verify, ok := msg.(ActionChangeAPIKeyState); ok {
			gotResult = true
			// The snapshotted key ("sk-original") passes the prefix
			// check; the pasted one ("nope") would not.
			require.Equal(t, APIKeyInputStateVerified, verify.State)
		}
	}
	require.True(t, gotResult, "expected the verify cmd to produce ActionChangeAPIKeyState")
}

// TestActionChangeAPIKeyState_IsDialogAddressed is the regression test for
// a verify result being delivered to the wrong dialog. Without a DialogID,
// Overlay.Update hands ActionChangeAPIKeyState to whichever dialog is on
// top (e.g. a permission prompt raised mid-verify) instead of routing it
// back to the still-open APIKeyInput, leaving that dialog stuck in
// APIKeyInputStateVerifying.
func TestActionChangeAPIKeyState_IsDialogAddressed(t *testing.T) {
	addressed, ok := any(ActionChangeAPIKeyState{}).(DialogAddressed)
	require.True(t, ok, "ActionChangeAPIKeyState must implement DialogAddressed")
	require.Equal(t, APIKeyInputID, addressed.DialogID())
}

// TestAPIKeyInput_EscCancelsWhileVerifying is the regression test for the
// only other way out of a stuck verify: previously
// APIKeyInputStateVerifying absorbed every key, esc included, so a
// verification that never resolved (or resolved into an unaddressed
// message under a dialog that had since opened over it) left restart as
// the only recovery.
func TestAPIKeyInput_EscCancelsWhileVerifying(t *testing.T) {
	com, _ := newAPIKeyTestCommon(t)
	provider := catwalk.Provider{ID: catwalk.InferenceProvider("test-provider"), Name: "Test Provider"}
	dlg, _ := NewAPIKeyInput(com, false, provider, nil)

	dlg.HandleMsg(ActionChangeAPIKeyState{State: APIKeyInputStateVerifying})
	require.Equal(t, APIKeyInputStateVerifying, dlg.state)

	action := dlg.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	_, ok := action.(ActionClose)
	require.True(t, ok, "esc must close the dialog even mid-verify, got %#v", action)
}
