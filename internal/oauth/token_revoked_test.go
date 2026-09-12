package oauth

import "testing"

func TestIsRefreshTokenRevokedRecognizesDeadTokenCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"invalid_grant", `{"error":"invalid_grant"}`, true},
		{"revoked", `{"error":"token revoked"}`, true},
		{"openai reused", `{"error":{"message":"Your refresh token has already been used to generate a new access token. Please try signing in again.","type":"invalid_request_error","code":"refresh_token_reused"}}`, true},
		{"openai expired", `{"error":{"code":"refresh_token_expired"}}`, true},
		{"openai invalidated", `{"error":{"code":"refresh_token_invalidated"}}`, true},
		{"server error", `{"error":"internal_error"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := &TokenExchangeError{StatusCode: 401, Body: tc.body}
			if got := err.IsRefreshTokenRevoked(); got != tc.want {
				t.Fatalf("IsRefreshTokenRevoked() = %v, want %v", got, tc.want)
			}
		})
	}
}
