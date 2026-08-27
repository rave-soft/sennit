package providerload

import (
	"context"
	"os"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/stretchr/testify/require"
)

// rotationConfig builds a *config.Config with a single provider entry
// carrying rotation, for validateRotationConfigs to check directly.
func rotationConfig(providerID string, rotation config.RotationConfig) *config.Config {
	return &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			providerID: {Rotation: &rotation},
		}),
	}
}

// problemContaining reports whether any problem for subject contains
// substr in its message.
func problemContaining(problems []config.Problem, subject, substr string) bool {
	for _, p := range problems {
		if p.Subject == subject && strings.Contains(p.Message, substr) {
			return true
		}
	}
	return false
}

// TestValidateRotationConfigs_ThresholdOnWrongProviderRejected pins §4 of
// the plan: min_remaining_percent only makes sense for a provider that
// reports remaining allowance (RotateThreshold). "opencode" isn't in the
// capabilities registry, so it defaults to RotateRateLimit and has no
// allowance to measure a threshold against.
func TestValidateRotationConfigs_ThresholdOnWrongProviderRejected(t *testing.T) {
	cfg := rotationConfig("opencode", config.RotationConfig{MinRemainingPercent: 20})
	New().validateRotationConfigs(cfg)

	provider, ok := cfg.Providers.Get("opencode")
	require.True(t, ok)
	require.Zero(t, provider.Rotation.MinRemainingPercent, "the bad field must be cleared, not the whole provider")
	require.True(t, problemContaining(config.Doctor(cfg), "opencode", "does not report how much of its limit is left"))
}

// TestValidateRotationConfigs_CooldownOnWrongProviderRejected is the
// symmetric case: cooldown is meaningless for "codex", which reports a
// real remaining-allowance number and rotates on that threshold instead.
func TestValidateRotationConfigs_CooldownOnWrongProviderRejected(t *testing.T) {
	cfg := rotationConfig("codex", config.RotationConfig{Cooldown: "5m"})
	New().validateRotationConfigs(cfg)

	provider, ok := cfg.Providers.Get("codex")
	require.True(t, ok)
	require.Empty(t, provider.Rotation.Cooldown)
	require.True(t, problemContaining(config.Doctor(cfg), "codex", "rotation for it triggers on that threshold"))
}

// TestValidateRotationConfigs_ThresholdOutOfRangeRejected covers the plain
// range check on a provider that does have the field available.
func TestValidateRotationConfigs_ThresholdOutOfRangeRejected(t *testing.T) {
	cfg := rotationConfig("codex", config.RotationConfig{MinRemainingPercent: 100})
	New().validateRotationConfigs(cfg)

	provider, ok := cfg.Providers.Get("codex")
	require.True(t, ok)
	require.Zero(t, provider.Rotation.MinRemainingPercent)
	require.True(t, problemContaining(config.Doctor(cfg), "codex", "must be between 1 and 99"))
}

// TestValidateRotationConfigs_CooldownInvalidDurationRejected covers the
// format check on a provider where cooldown is otherwise the right field.
func TestValidateRotationConfigs_CooldownInvalidDurationRejected(t *testing.T) {
	cfg := rotationConfig("opencode", config.RotationConfig{Cooldown: "not-a-duration"})
	New().validateRotationConfigs(cfg)

	provider, ok := cfg.Providers.Get("opencode")
	require.True(t, ok)
	require.Empty(t, provider.Rotation.Cooldown)
	require.True(t, problemContaining(config.Doctor(cfg), "opencode", "must be a positive duration"))
}

// TestValidateRotationConfigs_ValidSettingsUntouched makes sure the
// validator only clears what's actually wrong: a correctly-shaped
// RotateThreshold config on codex and a correctly-shaped RotateRateLimit
// config on opencode both pass through unchanged, with no problems raised.
func TestValidateRotationConfigs_ValidSettingsUntouched(t *testing.T) {
	cfg := &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			"codex":    {Rotation: &config.RotationConfig{Enabled: true, MinRemainingPercent: 15}},
			"opencode": {Rotation: &config.RotationConfig{Enabled: true, Cooldown: "10m"}},
		}),
	}
	New().validateRotationConfigs(cfg)

	codexProvider, ok := cfg.Providers.Get("codex")
	require.True(t, ok)
	require.Equal(t, 15, codexProvider.Rotation.MinRemainingPercent)

	openCodeProvider, ok := cfg.Providers.Get("opencode")
	require.True(t, ok)
	require.Equal(t, "10m", openCodeProvider.Rotation.Cooldown)

	require.Empty(t, config.Doctor(cfg))
}

// TestLoaderValidatesRotationForCatalogAndCustomProviders proves rotation
// validation actually runs for BOTH catalog-backed providers
// (mergeCatalogProviders' output) and custom ones (validateCustomProviders'
// output), by driving the same two merge functions Process uses and then
// the rotation pass, exactly as Process itself sequences them. A version of
// this check that only exercised one of the two merge paths would have
// missed a validator wired into just the other.
func TestLoaderValidatesRotationForCatalogAndCustomProviders(t *testing.T) {
	cfg := &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			// "azure" is catalog-backed (see the known list below) and,
			// like any provider absent from the accounts capabilities
			// registry, defaults to RotateRateLimit — so a threshold has
			// nothing to measure against.
			"azure": {Rotation: &config.RotationConfig{MinRemainingPercent: 50}},
			// "local" is a custom provider, also RotateRateLimit by
			// default; its cooldown is malformed rather than misplaced.
			"local": {
				BaseURL: "http://127.0.0.1:1/v1",
				Models:  []catwalk.Model{{ID: "local-model"}},
				Rotation: &config.RotationConfig{
					Cooldown: "not-a-duration",
				},
			},
		}),
	}
	environment := testEnvironment{"AZURE_ENDPOINT": "https://azure.test", "AZURE_OPENAI_API_VERSION": "2026-01-01"}
	l := New()
	known, err := l.mergeCatalogProviders(cfg, nil, environment, environment,
		[]catwalk.Provider{{ID: catwalk.InferenceProviderAzure, APIEndpoint: "$AZURE_ENDPOINT", Models: []catwalk.Model{{ID: "model"}}}},
		"", os.Stat)
	require.NoError(t, err)
	require.NoError(t, l.validateCustomProviders(cfg, known, environment, nil, t.TempDir()))
	l.validateRotationConfigs(cfg)

	azure, ok := cfg.Providers.Get("azure")
	require.True(t, ok)
	require.NotNil(t, azure.Rotation)
	require.Zero(t, azure.Rotation.MinRemainingPercent, "catalog provider's bad rotation field must be cleared")

	local, ok := cfg.Providers.Get("local")
	require.True(t, ok)
	require.NotNil(t, local.Rotation)
	require.Empty(t, local.Rotation.Cooldown, "custom provider's bad rotation field must be cleared")

	problems := config.Doctor(cfg)
	require.True(t, problemContaining(problems, "azure", "does not report how much of its limit is left"))
	require.True(t, problemContaining(problems, "local", "must be a positive duration"))
}

// TestLoaderProcessValidatesRotation drives the actual public entry point
// (Loader.Process, config-loading's real caller) rather than its internal
// merge/validate steps individually, so removing the
// validateRotationConfigs call from Process itself — not just breaking a
// rule inside it — is caught too.
func TestLoaderProcessValidatesRotation(t *testing.T) {
	cfg := customConfig(config.ProviderConfig{
		BaseURL:            "http://127.0.0.1:1/v1",
		AutoDiscoverModels: pointer(false),
		Models:             []catwalk.Model{{ID: "local-model"}},
		Rotation:           &config.RotationConfig{MinRemainingPercent: 20},
	})
	_, err := New().Process(context.Background(), config.RuntimeInput{Config: cfg})
	require.NoError(t, err)

	provider, ok := cfg.Providers.Get("local")
	require.True(t, ok)
	require.NotNil(t, provider.Rotation)
	require.Zero(t, provider.Rotation.MinRemainingPercent, "Process must run rotation validation, not just the merge steps")
	require.True(t, problemContaining(config.Doctor(cfg), "local", "does not report how much of its limit is left"))
}
