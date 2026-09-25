package workspace

import (
	"context"
	"errors"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/wireerr"
)

// ErrDiscoveryDisabled mirrors modelsrefresh.ErrDiscoveryDisabled: a
// provider's model discovery is disabled in its config
// (discover_models: false). It is a distinct sentinel from
// modelsrefresh.ErrDiscoveryDisabled rather than an alias of it, because
// this package must not import internal/modelsrefresh — that package
// transitively imports internal/modelcache, which imports internal/db,
// and this package's own doc comment (see workspace.go) forbids dragging
// internal/db into internal/ui's transitive closure. RefreshProviderModels'
// implementation (internal/workspace/appws) does import modelsrefresh, so
// it recognizes modelsrefresh.ErrDiscoveryDisabled with errors.Is and
// encodes it as the "discovery_disabled" code directly, bypassing
// EncodeError for that one value; DecodeError hands the code back as this
// sentinel instead. A caller that reaches a workspace only through the
// wire (the client/server split CLIENT-SERVER.md plans) and needs to
// recognize a disabled-discovery result should match on this sentinel,
// not modelsrefresh's.
var ErrDiscoveryDisabled = errors.New("discovery disabled (discover_models: false); define models in the config")

// sentinelCode pairs a wire code with the single sentinel error it stands
// for. sentinelCodes is the one ordered list both EncodeError and
// DecodeError work from — see its doc comment for why a second, separately
// maintained list (a map for lookup, a slice for iteration order) is what
// this replaces.
type sentinelCode struct {
	code     string
	sentinel error
}

// sentinelCodes is the single source of truth for every wire code backed
// by exactly one sentinel error ("read_only" and "provider_quota" are not
// here: their identity is structural, carried in wireerr.Error.ReadOnly/
// Quota, not a single sentinel — see EncodeError/DecodeError's handling of
// those two codes directly). EncodeError walks it in order with errors.Is
// to classify err; sentinelCodeMap, built from it once at init, is what
// DecodeError and TestEveryWorkspaceSentinelHasACode look codes/sentinels
// up in. Previously these were a map (for DecodeError's lookup) and a
// separately maintained slice (for EncodeError's iteration order); a code
// added to one and not the other compiled fine and was never produced or
// never decoded, and nothing caught it. One list closes that gap: adding a
// code here is enough for both directions to see it.
var sentinelCodes = []sentinelCode{
	{"canceled", context.Canceled},
	{"deadline_exceeded", context.DeadlineExceeded},
	{"session_not_found", session.ErrNotFound},
	{"agent_not_initialized", ErrAgentNotInitialized},
	{"server_unreachable", ErrServerUnreachable},
	{"workspace_gone", ErrWorkspaceGone},
	{"stream_closed", ErrStreamClosed},
	{"threads_not_supported", ErrThreadsNotSupported},
	{"tasks_not_supported", ErrTasksNotSupported},
	{"discovery_disabled", ErrDiscoveryDisabled},
}

// sentinelCodeMap indexes sentinelCodes by code, built once so DecodeError
// doesn't do a linear scan per call.
var sentinelCodeMap = func() map[string]error {
	m := make(map[string]error, len(sentinelCodes))
	for _, sc := range sentinelCodes {
		m[sc.code] = sc.sentinel
	}
	return m
}()

// EncodeError converts err into its wire representation. nil in, nil out.
// A code from sentinelCodes is chosen when err (or something it wraps, per
// errors.Is) matches that code's sentinel; failing that, "read_only" or
// "provider_quota" when err matches those structurally (errors.As); anything
// else becomes "internal", with the original message preserved but no
// identity to recover on the other side.
func EncodeError(err error) *wireerr.Error {
	if err == nil {
		return nil
	}
	msg := err.Error()

	for _, sc := range sentinelCodes {
		if errors.Is(err, sc.sentinel) {
			return &wireerr.Error{Code: sc.code, Message: msg}
		}
	}

	var roErr *ErrReadOnlyOperation
	if errors.As(err, &roErr) {
		return &wireerr.Error{
			Code: "read_only", Message: msg,
			ReadOnly: &wireerr.ReadOnly{Operation: roErr.Operation, Reason: roErr.Reason},
		}
	}

	if info, ok := GetProviderQuotaInfo(err); ok {
		return &wireerr.Error{
			Code: "provider_quota", Message: msg,
			Quota: &wireerr.Quota{Model: info.Model, SettingsURL: info.SettingsURL},
		}
	}

	return &wireerr.Error{Code: "internal", Message: msg}
}

// wrappedError is what DecodeError hands back for a code backed by a
// single sentinel: Error() reproduces the original message exactly (it may
// be a wrapped form, e.g. "listing sessions: %w"-around
// session.ErrNotFound, not the sentinel's own text), while Unwrap exposes
// the sentinel so errors.Is/errors.As on the decoded value answer exactly
// as they did on the original.
type wrappedError struct {
	msg    string
	target error
}

func (e *wrappedError) Error() string { return e.msg }
func (e *wrappedError) Unwrap() error { return e.target }

// decodedQuotaError reconstructs the interface workspace.GetProviderQuotaInfo
// matches on (see providerQuotaError in workspace.go) from a decoded
// wireerr.Quota, so a caller that went through the wire gets the same
// answer a caller talking to an in-process workspace would.
type decodedQuotaError struct {
	model, settingsURL string
}

func (e *decodedQuotaError) Error() string { return "provider quota exceeded" }
func (e *decodedQuotaError) QuotaInfo() (model, settingsURL string) {
	return e.model, e.settingsURL
}

// DecodeError reverses EncodeError. nil in, nil out. For every code
// EncodeError produces, the result's Error() equals the original error's,
// and errors.Is/errors.As/GetProviderQuotaInfo/IsReadOnlyError answer the
// decoded value exactly as they answered the original — see
// TestEncodeDecodeError_PreservesIdentity. An unrecognized code (from a
// future server this build doesn't fully understand) decodes to a plain
// error carrying the message, same as "internal".
func DecodeError(e *wireerr.Error) error {
	if e == nil {
		return nil
	}
	switch e.Code {
	case "read_only":
		var op, reason string
		if e.ReadOnly != nil {
			op, reason = e.ReadOnly.Operation, e.ReadOnly.Reason
		}
		return &wrappedError{msg: e.Message, target: &ErrReadOnlyOperation{Operation: op, Reason: reason}}
	case "provider_quota":
		var model, settingsURL string
		if e.Quota != nil {
			model, settingsURL = e.Quota.Model, e.Quota.SettingsURL
		}
		return &wrappedError{msg: e.Message, target: &decodedQuotaError{model: model, settingsURL: settingsURL}}
	}
	if sentinel, ok := sentinelCodeMap[e.Code]; ok {
		return &wrappedError{msg: e.Message, target: sentinel}
	}
	return errors.New(e.Message)
}
