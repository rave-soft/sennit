package proto

import (
	"encoding/json"
	"fmt"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/oauth"
)

// APIKeyKind discriminates the kind of credential carried in a
// ConfigProviderKeyRequest. JSON's `any` loses Go type information, so
// the wire format names the kind explicitly and the server decodes
// APIKey accordingly.
type APIKeyKind string

const (
	// APIKeyKindString is a plain string API key.
	APIKeyKindString APIKeyKind = "string"
	// APIKeyKindOAuth is an oauth.Token credential.
	APIKeyKindOAuth APIKeyKind = "oauth"
)

// ConfigProviderKeyRequest represents a request to set a provider API
// key. APIKey is the raw JSON for the credential; Kind selects the
// concrete Go type APIKey should be decoded into via DecodeAPIKey.
type ConfigProviderKeyRequest struct {
	Scope      config.Scope    `json:"scope"`
	ProviderID string          `json:"provider_id"`
	Kind       APIKeyKind      `json:"kind"`
	APIKey     json.RawMessage `json:"api_key"`
}

// DecodeAPIKey decodes APIKey into the Go type indicated by Kind. It
// returns a string for APIKeyKindString and a *oauth.Token for
// APIKeyKindOAuth. An unknown kind or malformed payload is reported
// as an error.
func (r ConfigProviderKeyRequest) DecodeAPIKey() (any, error) {
	switch r.Kind {
	case APIKeyKindString:
		var s string
		if err := json.Unmarshal(r.APIKey, &s); err != nil {
			return nil, fmt.Errorf("decode api key string: %w", err)
		}
		return s, nil
	case APIKeyKindOAuth:
		var tok oauth.Token
		if err := json.Unmarshal(r.APIKey, &tok); err != nil {
			return nil, fmt.Errorf("decode api key oauth token: %w", err)
		}
		return &tok, nil
	default:
		return nil, fmt.Errorf("unsupported api key kind %q", r.Kind)
	}
}
