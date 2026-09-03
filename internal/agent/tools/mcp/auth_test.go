package mcp

import (
	"errors"
	"testing"

	mcpoauth "github.com/rave-soft/sennit/internal/oauth/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// TestIsOAuthInitErr covers the error shapes that must classify an MCP
// connection failure as needs-auth (recoverable by re-authenticating)
// rather than a generic error.
func TestIsOAuthInitErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil-ish generic error", errors.New("connection refused"), false},
		{"interactive auth required", mcpoauth.ErrInteractiveAuthRequired, true},
		{"invalid_grant retrieve error", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, true},
		{"invalid_client retrieve error", &oauth2.RetrieveError{ErrorCode: "invalid_client"}, true},
		{"unrelated retrieve error", &oauth2.RetrieveError{ErrorCode: "server_error"}, false},
		{"no token available", errors.New("no token available"), true},
		{
			// This is the shape oauth2's tokenRefresher.Token returns when
			// a restored token has no refresh token and its access token
			// has expired: the request never goes out, so no 401 arrives
			// to drive the normal re-authorization flow. Matching on the
			// durable "refresh token is not set" substring, rather than
			// the full "oauth2: token expired and ..." string, tolerates
			// a reworded prefix.
			"expired token with no refresh token",
			errors.New("oauth2: token expired and refresh token is not set"),
			true,
		},
		{"unrelated error text", errors.New("dial tcp: connection reset"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isOAuthInitErr(tt.err))
		})
	}
}
