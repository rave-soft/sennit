// Package codex provides OpenAI Codex integration: signing in with a
// ChatGPT subscription and talking to the Codex backend that subscription
// unlocks.
//
// Codex is not an API-key provider. The models a Plus/Pro/Business plan
// includes live behind chatgpt.com's own Codex endpoint, which authenticates
// with a ChatGPT OAuth access token plus the account the token belongs to,
// and speaks the OpenAI Responses API. So this package covers the whole
// subscription path — the browser sign-in, token refresh, importing an
// existing Codex CLI login, and the model list that endpoint publishes —
// while the request/response wire format stays plain Responses API and is
// handled by the shared OpenAI provider.
//
// Two things about that endpoint differ from api.openai.com and are worth
// knowing before changing anything here: it refuses a non-streaming request
// outright ("Stream must be set to true"), which costs Sennit nothing since
// every model call it makes is a stream, and it requires a client_version
// on the model list. TestLiveAuthAndModels and TestLiveCodexStream (both
// gated on CODEX_LIVE=1) are how those facts stay honest.
package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// ProviderID is the provider's identifier in config and in the model
	// catalog. Codex is absent from the embedded catwalk catalog (it is not
	// a public API-key provider), so Sennit declares it itself.
	ProviderID = "codex"

	// ProviderName is how the provider is shown in the UI.
	ProviderName = "OpenAI Codex"

	// APIBaseURL is the Codex endpoint a ChatGPT subscription unlocks. It
	// speaks the Responses API, so /responses and /models hang off it
	// exactly as they do on api.openai.com/v1.
	APIBaseURL = "https://chatgpt.com/backend-api/codex"

	// clientID is the Codex OAuth client. It is a public client (no
	// secret): the whole flow is PKCE, which is why this can ship in a
	// binary at all.
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	authorizeURL = "https://auth.openai.com/oauth/authorize"
	tokenURL     = "https://auth.openai.com/oauth/token"

	// callbackPort and callbackPath together form the one redirect URI the
	// OAuth client accepts. Neither is negotiable: the port cannot fall
	// back to :0 the way an ordinary loopback flow would, because the
	// authorization server only redirects to this exact URI.
	callbackPort = 1455
	callbackPath = "/auth/callback"

	// scopes are what the Codex backend expects on the token. offline_access
	// is what makes a refresh token come back, without which every restart
	// would need the browser again.
	scopes = "openid profile email offline_access"

	// originator identifies the client to the Codex backend. The endpoint
	// is a first-party one, and requests without a known originator are
	// rejected outright.
	originator = "codex_cli_rs"

	// clientVersion is the Codex client version reported to the backend,
	// which the model list requires as a query parameter and refuses the
	// request without. It gates which models are offered, so it wants
	// bumping when the backend starts publishing models a newer client
	// knows about.
	clientVersion = "0.147.0"
)

// RedirectURI is the loopback address the browser is sent back to.
func RedirectURI() string {
	return fmt.Sprintf("http://localhost:%d%s", callbackPort, callbackPath)
}

// Headers returns the headers every Codex backend request needs beyond
// Authorization, given the account the token belongs to. accountID may be
// empty — the backend then falls back to the account encoded in the token
// itself, which is right for personal plans and wrong for anyone with more
// than one workspace, so callers should pass it when they know it.
func Headers(accountID string) map[string]string {
	headers := map[string]string{
		"originator":  originator,
		"OpenAI-Beta": "responses=experimental",
	}
	if accountID != "" {
		headers["chatgpt-account-id"] = accountID
	}
	return headers
}

// AccountID extracts the ChatGPT account ID from a Codex JWT (either the ID
// token or the access token — both carry the claim). It returns "" when the
// token is opaque or the claim is missing, which callers treat as "let the
// backend decide" rather than as an error.
func AccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Auth.AccountID
}
