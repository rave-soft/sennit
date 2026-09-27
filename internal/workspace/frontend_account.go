package workspace

import (
	"time"

	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// FrontendAccount is the allowlist projection of accounts.Account handed
// to a frontend. accounts.Account carries an OAuth token (access and
// refresh) and an API key template - both credentials that must never
// cross the wire once Workspace is served over gRPC with a JSON codec
// (see CLIENT-SERVER.md PR 0.5c). The UI only ever reads a token's expiry
// (internal/ui/dialog/accounts.go's tokenStatus), so that's all this type
// carries: HasToken plus the two expiry fields IsTokenExpired needs,
// never AccessToken/RefreshToken/Client. Likewise HasAPIKey replaces the
// APIKey template itself.
type FrontendAccount struct {
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	Email     string `json:"email,omitempty"`

	// ProxyURL has any userinfo password stripped - see RedactProxyURL.
	ProxyURL string         `json:"proxy_url,omitempty"`
	Disabled bool           `json:"disabled,omitempty"`
	Usage    accounts.Usage `json:"usage"`

	HasToken       bool  `json:"has_token,omitempty"`
	TokenExpiresAt int64 `json:"token_expires_at,omitempty"`
	TokenExpiresIn int   `json:"token_expires_in,omitempty"`
	HasAPIKey      bool  `json:"has_api_key,omitempty"`
}

// NewFrontendAccount projects a, redacting its credential down to the
// presence/expiry facts a frontend is allowed to see.
func NewFrontendAccount(a accounts.Account) FrontendAccount {
	fa := FrontendAccount{
		ID:        a.ID,
		Label:     a.Label,
		AccountID: a.AccountID,
		Email:     a.Email,
		ProxyURL:  RedactProxyURL(a.ProxyURL),
		Disabled:  a.Disabled,
		Usage:     a.Usage,
		HasAPIKey: a.APIKey != "",
	}
	if a.Token != nil {
		fa.HasToken = true
		fa.TokenExpiresAt = a.Token.ExpiresAt
		fa.TokenExpiresIn = a.Token.ExpiresIn
	}
	return fa
}

// tokenExpiryBuffer mirrors oauth.Token's own minRefreshBuffer: the
// minimum number of seconds before actual expiry at which a token counts
// as expired. It is duplicated here, rather than exported from
// internal/oauth, because that package's Token itself must never reach a
// frontend - see this file's doc comment - so there is nothing frontend
// code could import it alongside.
const tokenExpiryBuffer = 30

// IsTokenExpired reports whether the account's OAuth token is expired or
// about to expire, mirroring oauth.Token.IsExpired's buffer computation
// exactly (max(expires_in/10, 30s)) so the dialog's status text does not
// change when it switches from reading *oauth.Token to FrontendAccount.
// False for an account with no token at all.
func (a FrontendAccount) IsTokenExpired() bool {
	if !a.HasToken {
		return false
	}
	buffer := max(int64(a.TokenExpiresIn)/10, tokenExpiryBuffer)
	return time.Now().Unix() >= (a.TokenExpiresAt - buffer)
}

// TokenExpiresSoon mirrors the second half of the accounts dialog's old
// tokenStatus: within the same refresh buffer IsTokenExpired uses. It is
// kept as its own method, computed the same way, purely to preserve that
// dialog's exact behavior across the FrontendAccount switch - see its doc
// comment for why the two checks currently agree.
func (a FrontendAccount) TokenExpiresSoon() bool {
	if !a.HasToken {
		return false
	}
	buffer := max(int64(a.TokenExpiresIn)/10, tokenExpiryBuffer)
	return time.Now().Unix() >= a.TokenExpiresAt-buffer
}
