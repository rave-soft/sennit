package dialog

import (
	"errors"
	"image"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// providerSettingsTestWorkspace is a minimal [workspace.Workspace] stub,
// mirroring accountsTestWorkspace: NewProviderSettings only reads Config().
type providerSettingsTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

// KnownProviders mirrors what the UI used to compute for itself:
// the embedded catalog for this fake's config.
func (w providerSettingsTestWorkspace) KnownProviders() []catwalk.Provider {
	return providerruntime.Providers(w.cfg.Options.DisableDefaultProviders)
}

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w providerSettingsTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w providerSettingsTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w providerSettingsTestWorkspace) BuiltinSkills() []*skills.Skill {
	return skills.DiscoverBuiltin()
}

func (w *providerSettingsTestWorkspace) Config() *config.Config { return w.cfg }

// RuntimeProvider returns the provider's resolved credentials for the
// auth-state read in loadAuthStateCmd. The test config carries whatever
// the test set via cfg.Providers, so we just look it up there.
func (w *providerSettingsTestWorkspace) RuntimeProvider(providerID string) (config.ProviderConfig, bool) {
	pc, ok := w.cfg.Providers.Get(providerID)
	return pc, ok
}

// newProviderSettingsTestCommon builds a *common.Common whose Config()
// carries providerID with pc as its entry.
func newProviderSettingsTestCommon(t *testing.T, providerID string, pc config.ProviderConfig) *common.Common {
	t.Helper()
	s := styles.SennitDark()
	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}
	pc.ID = providerID
	cfg.Providers.Set(providerID, pc)
	ws := &providerSettingsTestWorkspace{cfg: cfg}
	return &common.Common{Styles: &s, Workspace: ws}
}

func typeIntoProviderSettings(t *testing.T, m *ProviderSettings, s string) {
	t.Helper()
	for _, r := range s {
		action := m.HandleMsg(keyMsg(r))
		require.Nil(t, action)
	}
}

func ctrlAMsg() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}
}

// customProviderSettings opens the dialog for a custom provider with a
// base_url, the only kind a model refresh applies to, focused on the
// Enabled field where the refresh key is bound.
func customProviderSettings(t *testing.T) *ProviderSettings {
	t.Helper()
	com := newProviderSettingsTestCommon(t, "custom", config.ProviderConfig{BaseURL: "http://127.0.0.1:9/v1"})
	m := newProviderSettings(com, "custom", workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})
	m.advanceFocus(1)
	return m
}

// drawProviderSettings renders m and returns the screen text.
func drawProviderSettings(m *ProviderSettings) string {
	area := image.Rect(0, 0, 80, 30)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	m.Draw(scr, area)
	return scr.String()
}

// TestProviderSettings_RefreshModelsReturnsActionWithoutWorkspaceIO proves
// the dialog only requests the side effect; the model runs it in a tea.Cmd.
func TestProviderSettings_RefreshModelsReturnsActionWithoutWorkspaceIO(t *testing.T) {
	t.Parallel()

	m := customProviderSettings(t)

	action := m.HandleMsg(keyMsg('r'))
	refresh, ok := action.(ActionRefreshModels)
	require.True(t, ok, "expected ActionRefreshModels, got %#v", action)
	require.Equal(t, "custom", refresh.ProviderID)
	require.Nil(t, m.HandleMsg(keyMsg('r')), "busy refresh must not trigger twice")
	require.Contains(t, drawProviderSettings(m), "Refreshing models…")

	require.Nil(t, m.HandleMsg(ActionRefreshModelsResult{
		ProviderID: "custom",
		Results:    []workspace.ModelRefreshResult{{ID: "custom", Models: 5, Added: 2, Removed: 1}},
	}))
	require.False(t, m.refreshing)
	require.Contains(t, drawProviderSettings(m), "Refreshed: 5 models (+2 new, -1 removed)")
}

// TestProviderSettings_SaveShowsSavingNotRefreshing pins the status line
// of a save: it shares the dialog with the refresh but not its label.
func TestProviderSettings_SaveShowsSavingNotRefreshing(t *testing.T) {
	t.Parallel()

	m := customProviderSettings(t)
	_, ok := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionSubmitProviderSettings)
	require.True(t, ok)

	screen := drawProviderSettings(m)
	require.Contains(t, screen, "Saving…")
	require.NotContains(t, screen, "Refreshing models…")
}

// TestProviderSettings_RefreshNotOfferedForCatalogProvider pins that a
// catalog provider, where the refresh can only fail, gets neither the
// key nor its help entry, and neither does a custom one without base_url.
func TestProviderSettings_RefreshNotOfferedForCatalogProvider(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, id string
		pc       config.ProviderConfig
	}{
		{"catalog", "openai", config.ProviderConfig{BaseURL: "https://api.openai.com/v1"}},
		{"no base_url", "custom", config.ProviderConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			com := newProviderSettingsTestCommon(t, tc.id, tc.pc)
			m := newProviderSettings(com, tc.id, workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})
			m.advanceFocus(1)

			require.Nil(t, m.HandleMsg(keyMsg('r')))
			require.False(t, m.refreshing)
			require.NotContains(t, m.ShortHelp(), m.keyMap.Refresh)
		})
	}
}

// TestProviderSettings_CloseDuringRefresh pins that Esc still closes the
// dialog while a refresh is in flight; other keys are ignored.
func TestProviderSettings_CloseDuringRefresh(t *testing.T) {
	t.Parallel()

	m := customProviderSettings(t)
	_, ok := m.HandleMsg(keyMsg('r')).(ActionRefreshModels)
	require.True(t, ok)

	require.Nil(t, m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}), "save must wait for the refresh")
	_, ok = m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}).(ActionClose)
	require.True(t, ok)
}

// TestProviderSettings_RefreshFailureShowsError pins that a provider's
// own failure lands on the error line, same as a request-level error.
func TestProviderSettings_RefreshFailureShowsError(t *testing.T) {
	t.Parallel()

	m := customProviderSettings(t)
	m.HandleMsg(keyMsg('r'))
	require.Nil(t, m.HandleMsg(ActionRefreshModelsResult{
		ProviderID: "custom",
		Results:    []workspace.ModelRefreshResult{{ID: "custom", Err: errors.New("endpoint down")}},
	}))
	require.Equal(t, "endpoint down", m.errMsg)
	require.Empty(t, m.refreshMsg)
}

// TestProviderSettings_RotateThreshold_ShowsThresholdNotCooldown covers
// the Codex-shaped case: the threshold field exists, the cooldown one
// does not.
func TestProviderSettings_RotateThreshold_ShowsThresholdNotCooldown(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})

	require.Equal(t, []providerSettingsField{
		providerSettingsFieldProxy, providerSettingsFieldEnabled, providerSettingsFieldThreshold,
	}, m.fields)
}

// TestProviderSettings_RotateRateLimit_ShowsCooldownNotThreshold is the
// symmetric case for a 429-triggered provider.
func TestProviderSettings_RotateRateLimit_ShowsCooldownNotThreshold(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "opencode", config.ProviderConfig{})
	m := newProviderSettings(com, "opencode", workspace.AccountCapabilities{RotateOn: workspace.RotateRateLimit})

	require.Equal(t, []providerSettingsField{
		providerSettingsFieldProxy, providerSettingsFieldEnabled, providerSettingsFieldCooldown,
	}, m.fields)
}

// TestProviderSettings_RotateBoth_ShowsThresholdAndCooldown covers the
// Codex-shaped case (RotateBoth): both the threshold and the cooldown
// field exist.
func TestProviderSettings_RotateBoth_ShowsThresholdAndCooldown(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{
		Rotation: &config.RotationConfig{Enabled: true, MinRemainingPercent: 10, Cooldown: "20m"},
	})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateBoth})

	require.Equal(t, []providerSettingsField{
		providerSettingsFieldProxy, providerSettingsFieldEnabled, providerSettingsFieldThreshold, providerSettingsFieldCooldown,
	}, m.fields)
	require.Equal(t, "10", m.threshold.Value())
	require.Equal(t, "20m", m.cooldown.Value())
}

// TestProviderSettings_RotateNever_NoRotationControls pins the requirement
// that a provider whose capabilities say rotation is never offered shows
// only the proxy field — no Enabled toggle, no threshold, no cooldown,
// either in the focusable field list or in what Draw renders.
func TestProviderSettings_RotateNever_NoRotationControls(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "solo", config.ProviderConfig{})
	m := newProviderSettings(com, "solo", workspace.AccountCapabilities{RotateOn: workspace.RotateNever})

	require.Equal(t, []providerSettingsField{providerSettingsFieldProxy}, m.fields,
		"a RotateNever provider must offer only the proxy field")
}

// TestProviderSettings_PrefillsProxyAndRotationFromConfig covers the
// form's initial values: the provider's ConfiguredProxyURL (not the
// possibly-account-overridden effective ProxyURL) and its stored
// Rotation settings.
func TestProviderSettings_PrefillsProxyAndRotationFromConfig(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{
		ProxyURL: "http://provider-proxy.example:8080",
		Rotation: &config.RotationConfig{Enabled: true, MinRemainingPercent: 25},
	})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})

	require.Equal(t, "http://provider-proxy.example:8080", m.proxy.Value())
	require.True(t, m.enabled)
	require.Equal(t, "25", m.threshold.Value())
}

// TestProviderSettings_InvalidProxyRejectedBeforeSaving mirrors
// AccountForm's own proxy validation test: a bad proxy value must not
// submit.
func TestProviderSettings_InvalidProxyRejectedBeforeSaving(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})
	typeIntoProviderSettings(t, m, "://not-a-url")

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action, "an invalid proxy must not submit")
	require.NotEmpty(t, m.errMsg)
	require.False(t, m.submitting)
}

// TestProviderSettings_ThresholdOutOfRangeRejectedBeforeSaving covers the
// same range check config validation applies, caught here before the
// value ever reaches config so the user sees the error immediately.
func TestProviderSettings_ThresholdOutOfRangeRejectedBeforeSaving(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})
	m.advanceFocus(2) // Proxy -> Enabled -> Threshold
	require.Equal(t, providerSettingsFieldThreshold, m.currentField())
	typeIntoProviderSettings(t, m, "150")

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.NotEmpty(t, m.errMsg)
}

// TestProviderSettings_CooldownInvalidRejectedBeforeSaving is the cooldown
// analogue of the threshold range check above.
func TestProviderSettings_CooldownInvalidRejectedBeforeSaving(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "opencode", config.ProviderConfig{})
	m := newProviderSettings(com, "opencode", workspace.AccountCapabilities{RotateOn: workspace.RotateRateLimit})
	m.advanceFocus(2) // Proxy -> Enabled -> Cooldown
	require.Equal(t, providerSettingsFieldCooldown, m.currentField())
	typeIntoProviderSettings(t, m, "not-a-duration")

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.NotEmpty(t, m.errMsg)
}

// TestProviderSettings_SubmitCarriesRotationOnlyWhenApplicable covers the
// wiring between caps.RotateOn and what ActionSubmitProviderSettings
// carries: RotateNever submits Rotation == nil so nothing is written for
// a provider with no rotation config at all; RotateThreshold and
// RotateRateLimit submit a populated RotationConfig with only the field
// that applies to them.
func TestProviderSettings_SubmitCarriesRotationOnlyWhenApplicable(t *testing.T) {
	t.Run("RotateNever", func(t *testing.T) {
		com := newProviderSettingsTestCommon(t, "solo", config.ProviderConfig{})
		m := newProviderSettings(com, "solo", workspace.AccountCapabilities{RotateOn: workspace.RotateNever})

		action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
		submit, ok := action.(ActionSubmitProviderSettings)
		require.True(t, ok, "expected ActionSubmitProviderSettings, got %#v", action)
		require.Nil(t, submit.Rotation)
	})

	t.Run("RotateThreshold", func(t *testing.T) {
		com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
		m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})
		m.advanceFocus(2)
		typeIntoProviderSettings(t, m, "15")

		action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
		submit, ok := action.(ActionSubmitProviderSettings)
		require.True(t, ok, "expected ActionSubmitProviderSettings, got %#v", action)
		require.NotNil(t, submit.Rotation)
		require.Equal(t, 15, submit.Rotation.MinRemainingPercent)
		require.Empty(t, submit.Rotation.Cooldown)
	})

	t.Run("RotateRateLimit", func(t *testing.T) {
		com := newProviderSettingsTestCommon(t, "opencode", config.ProviderConfig{})
		m := newProviderSettings(com, "opencode", workspace.AccountCapabilities{RotateOn: workspace.RotateRateLimit})
		m.advanceFocus(2)
		typeIntoProviderSettings(t, m, "15m")

		action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
		submit, ok := action.(ActionSubmitProviderSettings)
		require.True(t, ok, "expected ActionSubmitProviderSettings, got %#v", action)
		require.NotNil(t, submit.Rotation)
		require.Equal(t, "15m", submit.Rotation.Cooldown)
		require.Zero(t, submit.Rotation.MinRemainingPercent)
	})

	t.Run("RotateBoth", func(t *testing.T) {
		com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
		m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateBoth})
		m.advanceFocus(2) // Proxy -> Enabled -> Threshold
		typeIntoProviderSettings(t, m, "15")
		m.advanceFocus(1) // Threshold -> Cooldown
		typeIntoProviderSettings(t, m, "20m")

		action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
		submit, ok := action.(ActionSubmitProviderSettings)
		require.True(t, ok, "expected ActionSubmitProviderSettings, got %#v", action)
		require.NotNil(t, submit.Rotation)
		require.Equal(t, 15, submit.Rotation.MinRemainingPercent)
		require.Equal(t, "20m", submit.Rotation.Cooldown)
	})
}

// TestProviderSettings_SubmitPreservesAccountOrder pins that saving the
// form keeps a rotation order the form never offered. The save writes the
// rotation object whole, so a field the dialog does not know about is
// destroyed unless it is carried through deliberately.
func TestProviderSettings_SubmitPreservesAccountOrder(t *testing.T) {
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{
		Rotation: &config.RotationConfig{
			Enabled: true,
			Order:   []string{"acc_work", "acc_personal"},
		},
	})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateThreshold})

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	submit, ok := action.(ActionSubmitProviderSettings)
	require.True(t, ok, "expected ActionSubmitProviderSettings, got %#v", action)
	require.NotNil(t, submit.Rotation)
	require.Equal(t, []string{"acc_work", "acc_personal"}, submit.Rotation.Order)
}

// TestProviderSettings_AuthBadgeShowsSignedInWhenTokenPresent covers the
// auth-state read: a provider whose RuntimeProvider carries a valid
// OAuthToken must render the "signed in" badge.
func TestProviderSettings_AuthBadgeShowsSignedInWhenTokenPresent(t *testing.T) {
	t.Parallel()
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateBoth})

	// Simulate the async load delivering an OK state.
	m.HandleMsg(providerSettingsAuthLoadedMsg{providerID: "codex", state: providerSettingsAuthOK})
	require.Equal(t, providerSettingsAuthOK, m.authState)
	require.Contains(t, m.authBadge(), "signed in")
}

// TestProviderSettings_AuthBadgeShowsExpiredWhenTokenExpired covers the
// expired-token case: the badge must say "token expired".
func TestProviderSettings_AuthBadgeShowsExpiredWhenTokenExpired(t *testing.T) {
	t.Parallel()
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateBoth})

	m.HandleMsg(providerSettingsAuthLoadedMsg{providerID: "codex", state: providerSettingsAuthExpired})
	require.Equal(t, providerSettingsAuthExpired, m.authState)
	require.Contains(t, m.authBadge(), "token expired")
}

// TestProviderSettings_AuthBadgeHiddenForAPIKeyProvider covers the
// API-key case: authState stays Unknown, the badge is not rendered.
func TestProviderSettings_AuthBadgeHiddenForAPIKeyProvider(t *testing.T) {
	t.Parallel()
	com := newProviderSettingsTestCommon(t, "anthropic", config.ProviderConfig{APIKey: "sk-test"})
	m := newProviderSettings(com, "anthropic", workspace.AccountCapabilities{RotateOn: workspace.RotateNever})

	require.Equal(t, providerSettingsAuthUnknown, m.authState)
	require.Empty(t, m.authBadge())
}

// TestProviderSettings_PlainATypesIntoField pins that 'a' is a regular
// character in the text inputs: sign-in lives in the account edit form,
// not here.
func TestProviderSettings_PlainATypesIntoField(t *testing.T) {
	t.Parallel()
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateBoth})
	m.HandleMsg(providerSettingsAuthLoadedMsg{providerID: "codex", state: providerSettingsAuthMissing})

	action := m.HandleMsg(keyMsg('a'))
	_, isAdd := action.(ActionAddAccount)
	require.False(t, isAdd, "'a' must not trigger sign-in here")
	require.Equal(t, "a", m.proxy.Value())
}

// TestProviderSettings_CtrlADoesNotSignIn pins that ctrl+a is not bound
// in the provider settings dialog: sign-in moved to the account edit
// form. The chord falls through to the text input, which ignores it.
func TestProviderSettings_CtrlADoesNotSignIn(t *testing.T) {
	t.Parallel()
	com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
	m := newProviderSettings(com, "codex", workspace.AccountCapabilities{RotateOn: workspace.RotateBoth})
	m.HandleMsg(providerSettingsAuthLoadedMsg{providerID: "codex", state: providerSettingsAuthMissing})

	action := m.HandleMsg(ctrlAMsg())
	_, isAdd := action.(ActionAddAccount)
	require.False(t, isAdd, "ctrl+a must not trigger sign-in in provider settings")
	require.Empty(t, m.proxy.Value(), "ctrl+a must not type into the field either")
}

// TestProviderAuthState covers the classification the badge renders, which
// the badge tests above deliberately bypass by injecting a state. It reads
// the provider's live credential - the one requests go out with - and the
// four answers it can give are each a different thing to tell the user.
func TestProviderAuthState(t *testing.T) {
	t.Parallel()

	expired := &oauth.Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	valid := &oauth.Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()}

	tests := []struct {
		name string
		// runtime is the provider's live credential; nil means the
		// provider has no runtime entry at all.
		runtime *providerstate.Provider
		want    providerSettingsAuthState
	}{
		{
			name:    "a valid OAuth token is signed in",
			runtime: &providerstate.Provider{ID: "codex", OAuthToken: valid},
			want:    providerSettingsAuthOK,
		},
		{
			name:    "an expired OAuth token needs a refresh",
			runtime: &providerstate.Provider{ID: "codex", OAuthToken: expired},
			want:    providerSettingsAuthExpired,
		},
		{
			// Nothing to say: an API key has no expiry, and the key is
			// already visible in the settings themselves.
			name:    "an API key reports nothing",
			runtime: &providerstate.Provider{ID: "codex", APIKey: "sk-test"},
			want:    providerSettingsAuthUnknown,
		},
		{
			name:    "no credential at all is not signed in",
			runtime: &providerstate.Provider{ID: "codex"},
			want:    providerSettingsAuthMissing,
		},
		{
			name:    "a provider with no runtime entry reports nothing",
			runtime: nil,
			want:    providerSettingsAuthUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			com := newProviderSettingsTestCommon(t, "codex", config.ProviderConfig{})
			com.Config().RuntimeProviders = csync.NewMap[string, providerstate.Provider]()
			if tt.runtime != nil {
				com.Config().SetRuntimeProvider("codex", *tt.runtime)
			}
			require.Equal(t, tt.want, providerAuthState(com, "codex"))
		})
	}
}
