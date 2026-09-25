package workspace

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/stretchr/testify/require"
)

// TestNewFrontendAccount_ProjectsOnlyPresenceAndExpiry pins the shape a
// frontend actually receives for an OAuth account: it must carry
// HasToken/TokenExpiresAt/TokenExpiresIn, never the token itself.
func TestNewFrontendAccount_ProjectsOnlyPresenceAndExpiry(t *testing.T) {
	t.Parallel()

	a := accounts.Account{
		ID:        "acct-1",
		Label:     "Work",
		AccountID: "acct-remote-1",
		Email:     "person@example.com",
		ProxyURL:  "http://user:secret@proxy.example:8080",
		Token: &oauth.Token{
			AccessToken:  "access-secret",
			RefreshToken: "refresh-secret",
			ExpiresIn:    3600,
			ExpiresAt:    1234567890,
		},
		Disabled: true,
	}

	fa := NewFrontendAccount(a)
	require.Equal(t, "acct-1", fa.ID)
	require.Equal(t, "Work", fa.Label)
	require.Equal(t, "acct-remote-1", fa.AccountID)
	require.Equal(t, "person@example.com", fa.Email)
	require.Equal(t, "http://user@proxy.example:8080", fa.ProxyURL, "the proxy's password must be stripped")
	require.True(t, fa.Disabled)
	require.True(t, fa.HasToken)
	require.Equal(t, int64(1234567890), fa.TokenExpiresAt)
	require.Equal(t, 3600, fa.TokenExpiresIn)
	require.False(t, fa.HasAPIKey)
}

// TestNewFrontendAccount_APIKeyAccount covers the other credential shape:
// HasAPIKey true, HasToken false, and no token expiry fields set.
func TestNewFrontendAccount_APIKeyAccount(t *testing.T) {
	t.Parallel()

	a := accounts.Account{ID: "acct-2", APIKey: "$OPENAI_API_KEY"}
	fa := NewFrontendAccount(a)

	require.True(t, fa.HasAPIKey)
	require.False(t, fa.HasToken)
	require.Zero(t, fa.TokenExpiresAt)
	require.Zero(t, fa.TokenExpiresIn)
}

// TestFrontendAccount_TokenStatus drives IsTokenExpired/TokenExpiresSoon
// against a valid, an expiring-soon, and an expired token, and against an
// account with no token at all - the three cases
// internal/ui/dialog/accounts.go's tokenStatus renders as "valid",
// "expiring soon"/"expired", and (nothing, since it's gated on HasToken).
func TestFrontendAccount_TokenStatus(t *testing.T) {
	t.Parallel()

	valid := FrontendAccount{HasToken: true, TokenExpiresIn: 3600, TokenExpiresAt: time.Now().Add(time.Hour).Unix()}
	require.False(t, valid.IsTokenExpired())

	expired := FrontendAccount{HasToken: true, TokenExpiresIn: 3600, TokenExpiresAt: time.Now().Add(-time.Hour).Unix()}
	require.True(t, expired.IsTokenExpired())

	// Mirrors oauth.Token.IsExpired's own buffer: within
	// max(expires_in/10, 30s) of expiry counts as expired too.
	expiringSoon := FrontendAccount{HasToken: true, TokenExpiresIn: 3600, TokenExpiresAt: time.Now().Add(10 * time.Second).Unix()}
	require.True(t, expiringSoon.IsTokenExpired())

	noToken := FrontendAccount{}
	require.False(t, noToken.IsTokenExpired())
	require.False(t, noToken.TokenExpiresSoon())
}

// TestFrontendAccountJSON_NeverLeaksCredential is the secret check
// CLIENT-SERVER.md PR 0.5c calls for at the DTO level: marshaling a
// FrontendAccount built from an account with both a token and an api key
// must contain neither.
func TestFrontendAccountJSON_NeverLeaksCredential(t *testing.T) {
	t.Parallel()

	a := accounts.Account{
		ID: "acct-1",
		Token: &oauth.Token{
			AccessToken:  "top-secret-access-token",
			RefreshToken: "top-secret-refresh-token",
		},
	}
	data, err := json.Marshal(NewFrontendAccount(a))
	require.NoError(t, err)
	s := string(data)
	require.NotContains(t, s, "top-secret-access-token")
	require.NotContains(t, s, "top-secret-refresh-token")

	a2 := accounts.Account{ID: "acct-2", APIKey: "$SOME_SECRET_TEMPLATE"}
	data2, err := json.Marshal(NewFrontendAccount(a2))
	require.NoError(t, err)
	require.NotContains(t, string(data2), "SOME_SECRET_TEMPLATE")
}
