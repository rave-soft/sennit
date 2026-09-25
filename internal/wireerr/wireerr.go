// Package wireerr is the wire shape a Go error takes when it crosses a
// DTO boundary that must survive encoding/json: a plain `error` field
// encodes as `{}` and cannot be decoded back, so every DTO field that used
// to be typed `error` carries a *wireerr.Error instead. It is a leaf
// package (stdlib only) so it can be imported both by internal/workspace,
// which owns identity-preserving encode/decode of the errors it knows
// about (see workspace.EncodeError / workspace.DecodeError), and by
// internal/skills, which cannot import internal/workspace (workspace
// imports skills) and only needs a display-only conversion (see
// FromMessage).
package wireerr

// Quota carries the data workspace.GetProviderQuotaInfo reports, for an
// Error whose Code is "provider_quota".
type Quota struct {
	Model       string `json:"model"`
	SettingsURL string `json:"settings_url"`
}

// ReadOnly carries the data workspace.ErrReadOnlyOperation reports, for an
// Error whose Code is "read_only".
type ReadOnly struct {
	Operation string `json:"operation"`
	Reason    string `json:"reason"`
}

// Error is the wire representation of a Go error. Code identifies which
// sentinel (if any) it stands for, so a decoder can hand callers back
// something errors.Is/errors.As recognizes instead of an opaque string;
// Message is always the original error's Error() text. Quota and ReadOnly
// carry the structured payload some codes need beyond a message; both are
// nil unless Code calls for them.
type Error struct {
	Code     string    `json:"code"`
	Message  string    `json:"message"`
	Quota    *Quota    `json:"quota,omitempty"`
	ReadOnly *ReadOnly `json:"read_only,omitempty"`
}

// Text reports e's message, nil-safe: calling it on a nil *Error (easy to
// end up with when a DTO field carries no error) reports "" rather than
// panicking.
//
// This is deliberately not named Error(): that would give *Error an
// Error() string method and make it satisfy the error interface, and a
// nil *Error stored in an error-typed variable or field is a non-nil
// interface (the classic Go typed-nil trap) — exactly the failure mode
// this type exists to keep off the wire. A caller that needs an `error`
// value back, with the original's identity restored, must go through
// workspace.DecodeError; a caller that only wants to display the message
// calls Text() (or reads Message directly).
func (e *Error) Text() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// FromMessage builds a display-only *Error from err, tagged "internal"
// (no code this package's own consumers can key identity off). It exists
// for producers, like internal/skills, that build one of these DTO fields
// but cannot import internal/workspace for its identity-preserving
// EncodeError (workspace imports skills, so the reverse import would
// cycle). Consumers of a FromMessage-built Error should only ever display
// it, never test its identity. Nil in, nil out.
func FromMessage(err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{Code: "internal", Message: err.Error()}
}
