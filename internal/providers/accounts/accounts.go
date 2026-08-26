// Package accounts models multiple credentialed accounts per provider —
// the data needed to let a provider (Codex, Copilot, ...) rotate between
// several signed-in identities instead of the single credential the rest
// of Sennit assumes today.
//
// This is a leaf package: it depends on nothing but the standard library,
// internal/oauth (for the token shape), internal/lock, and internal/fsext.
// internal/config imports this package, never the other way around — that
// keeps the account store usable from anywhere config already reaches
// without creating an import cycle back into config itself.
//
// Accounts live in their own file rather than inside sennit.json because
// OAuth tokens for several accounts per provider would bloat a config file
// users are expected to read and hand-edit, and because token material
// churns (refreshes, usage snapshots) far more often than the rest of the
// config. Keeping it separate also means a corrupt or oversized account
// file can never break config parsing.
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rave-soft/sennit/internal/oauth"
)

// Account is one credentialed identity for a provider. A provider that
// supports multiple accounts (see Capabilities) keeps a list of these,
// keyed by ID, and rotates between them.
type Account struct {
	// ID is a stable identifier for this account within its provider.
	// It never changes once assigned, even if Label or Email do.
	ID string `json:"id"`
	// Label is the user-editable display name shown in the UI.
	Label string `json:"label,omitempty"`
	// AccountID is the provider's own identifier for the account — for
	// Codex, the chatgpt_account_id claim out of the OAuth JWT. It is
	// distinct from ID, which is Sennit's own bookkeeping key.
	AccountID string `json:"account_id,omitempty"`
	// Email is for display only and may be empty; nothing keys off it.
	Email string `json:"email,omitempty"`
	// ProxyURL overrides the provider's proxy for this account alone.
	// "" inherits the provider's proxy; "none" forces a direct
	// connection with no proxy at all.
	ProxyURL string `json:"proxy_url,omitempty"`
	// Token holds OAuth credentials, for providers authenticated that
	// way. Exactly one of Token and APIKey is set; see Validate.
	Token *oauth.Token `json:"token,omitempty"`
	// APIKey holds an API-key credential. It stores the template the
	// user configured ("$VAR" or "$(cmd)"), never the resolved secret
	// value — resolution happens where the key is used, not here.
	APIKey string `json:"api_key,omitempty"`
	// Disabled accounts are kept but skipped by rotation and selection.
	Disabled bool `json:"disabled,omitempty"`
	// Usage is the last known snapshot of this account's rate limits.
	// It is the zero value for providers that never report usage. Note:
	// encoding/json's omitempty does not look inside structs, so a zero
	// Usage still serializes as an explicit "usage" object rather than
	// being omitted; that's harmless and not worth fighting.
	Usage Usage `json:"usage"`
}

// Validate checks the invariants Upsert relies on: an ID is present, and
// exactly one of Token or APIKey is set. An account with neither
// credential can't authenticate; one with both is ambiguous about which
// to use.
func (a Account) Validate() error {
	if a.ID == "" {
		return errors.New("account id is required")
	}
	hasToken := a.Token != nil
	hasAPIKey := a.APIKey != ""
	switch {
	case !hasToken && !hasAPIKey:
		return fmt.Errorf("account %q has neither a token nor an api key", a.ID)
	case hasToken && hasAPIKey:
		return fmt.Errorf("account %q has both a token and an api key", a.ID)
	}
	return nil
}

// Usage is a provider-neutral snapshot of an account's rate limits. Its
// shape deliberately mirrors codex.Usage (internal/oauth/codex/usage.go)
// so a conversion between the two is a straight field copy; that
// conversion is added in a later phase, not here. It is not a type alias
// because this package must not import internal/oauth/codex — accounts is
// a leaf, and codex is a provider-specific package that should depend on
// shared types, not the reverse.
//
// The fields carry no json tags: Usage marshals through MarshalJSON and
// usageJSON below, so tags here would never be consulted. usageJSON is
// the single place the wire format is defined.
type Usage struct {
	// Plan is the subscription tier, as the provider spells it.
	Plan string
	// Primary and Secondary are the rate-limit windows the plan is
	// measured in, typically a long one and a short one. A window the
	// account does not have reports Known() false.
	Primary   UsageWindow
	Secondary UsageWindow
	// CapturedAt is when this snapshot was taken.
	CapturedAt time.Time
}

// UsageWindow is one rate-limit window. Like Usage, it carries no json
// tags: it reaches disk only through usageWindowJSON.
type UsageWindow struct {
	UsedPercent int
	// WindowMinutes is the window's length. Zero means the window does
	// not exist for this account, which Known reports.
	WindowMinutes int
	// ResetsAt is when the window rolls over. Zero when unknown.
	ResetsAt time.Time
}

// Known reports whether this window exists for the account.
func (w UsageWindow) Known() bool { return w.WindowMinutes > 0 }

// Known reports whether anything was captured at all.
func (u Usage) Known() bool { return u.Plan != "" || u.Primary.Known() }

// usageWindowJSON and usageJSON are the on-disk shadow of UsageWindow and
// Usage, with times as Unix seconds instead of time.Time. Both
// MarshalJSON and UnmarshalJSON share these so the wire format only has
// one place to change.
type usageWindowJSON struct {
	UsedPercent   int   `json:"used_percent,omitempty"`
	WindowMinutes int   `json:"window_minutes,omitempty"`
	ResetsAt      int64 `json:"resets_at,omitempty"`
}

type usageJSON struct {
	Plan       string          `json:"plan,omitempty"`
	Primary    usageWindowJSON `json:"primary"`
	Secondary  usageWindowJSON `json:"secondary"`
	CapturedAt int64           `json:"captured_at,omitempty"`
}

func toUsageWindowJSON(w UsageWindow) usageWindowJSON {
	var resets int64
	if !w.ResetsAt.IsZero() {
		resets = w.ResetsAt.Unix()
	}
	return usageWindowJSON{UsedPercent: w.UsedPercent, WindowMinutes: w.WindowMinutes, ResetsAt: resets}
}

func fromUsageWindowJSON(w usageWindowJSON) UsageWindow {
	uw := UsageWindow{UsedPercent: w.UsedPercent, WindowMinutes: w.WindowMinutes}
	if w.ResetsAt > 0 {
		uw.ResetsAt = time.Unix(w.ResetsAt, 0)
	}
	return uw
}

// MarshalJSON serializes times as Unix seconds, matching the convention
// used elsewhere in Sennit's persisted state (see AGENTS.md, Timestamps).
func (u Usage) MarshalJSON() ([]byte, error) {
	var captured int64
	if !u.CapturedAt.IsZero() {
		captured = u.CapturedAt.Unix()
	}
	return json.Marshal(usageJSON{
		Plan:       u.Plan,
		Primary:    toUsageWindowJSON(u.Primary),
		Secondary:  toUsageWindowJSON(u.Secondary),
		CapturedAt: captured,
	})
}

// UnmarshalJSON reads times back from Unix seconds.
func (u *Usage) UnmarshalJSON(data []byte) error {
	var a usageJSON
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	u.Plan = a.Plan
	u.Primary = fromUsageWindowJSON(a.Primary)
	u.Secondary = fromUsageWindowJSON(a.Secondary)
	if a.CapturedAt > 0 {
		u.CapturedAt = time.Unix(a.CapturedAt, 0)
	}
	return nil
}
