package accounts

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/oauth"
)

func TestAccount_ValidateNeitherCredential(t *testing.T) {
	t.Parallel()
	err := Account{ID: "a1"}.Validate()
	require.Error(t, err)
}

func TestAccount_ValidateBothCredentials(t *testing.T) {
	t.Parallel()
	err := Account{ID: "a1", APIKey: "$KEY", Token: &oauth.Token{AccessToken: "x"}}.Validate()
	require.Error(t, err)
}

func TestAccount_ValidateMissingID(t *testing.T) {
	t.Parallel()
	err := Account{APIKey: "$KEY"}.Validate()
	require.Error(t, err)
}

func TestAccount_ValidateOK(t *testing.T) {
	t.Parallel()
	require.NoError(t, Account{ID: "a1", APIKey: "$KEY"}.Validate())
	require.NoError(t, Account{ID: "a1", Token: &oauth.Token{AccessToken: "x"}}.Validate())
}

func TestCapabilitiesOf(t *testing.T) {
	t.Parallel()

	codex := CapabilitiesOf("codex")
	require.True(t, codex.Usage)
	require.Equal(t, RotateBoth, codex.RotateOn)
	require.True(t, codex.RotateOn.RotatesOnThreshold())
	require.True(t, codex.RotateOn.RotatesOnRateLimit())
	require.Equal(t, AuthOAuth, codex.AuthKind)

	copilot := CapabilitiesOf("copilot")
	require.False(t, copilot.Usage)
	require.Equal(t, RotateRateLimit, copilot.RotateOn)
	require.False(t, copilot.RotateOn.RotatesOnThreshold())
	require.True(t, copilot.RotateOn.RotatesOnRateLimit())
	require.Equal(t, AuthOAuth, copilot.AuthKind)

	unknown := CapabilitiesOf("some-unknown-provider")
	require.False(t, unknown.Usage)
	require.Equal(t, RotateRateLimit, unknown.RotateOn)
	require.Equal(t, AuthAPIKey, unknown.AuthKind)

	require.False(t, RotateNever.RotatesOnThreshold())
	require.False(t, RotateNever.RotatesOnRateLimit())
	require.True(t, RotateThreshold.RotatesOnThreshold())
	require.False(t, RotateThreshold.RotatesOnRateLimit())
	require.Equal(t, "never", RotateNever.String())
	require.Equal(t, "both", RotateBoth.String())
}
