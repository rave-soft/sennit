package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/wireerr"
	"github.com/stretchr/testify/require"
)

// localQuotaError is a stand-in for agent.ProviderQuotaError: this package
// cannot import internal/agent (see TestDomainPackageDoesNotDependOnAgentTransitively),
// so this is a local type satisfying the same providerQuotaError interface
// GetProviderQuotaInfo matches on.
type localQuotaError struct {
	model, settingsURL string
}

func (e *localQuotaError) Error() string { return "provider quota exceeded for " + e.model }
func (e *localQuotaError) QuotaInfo() (model, settingsURL string) {
	return e.model, e.settingsURL
}

// TestEncodeDecodeError_PreservesIdentity is the acceptance table: for
// every code EncodeError/DecodeError know about, round-tripping an error
// through the pair must reproduce its Error() string and every identity
// check a caller might run against it.
func TestEncodeDecodeError_PreservesIdentity(t *testing.T) {
	t.Parallel()

	roErr := &ErrReadOnlyOperation{Operation: "AgentRun", Reason: "thread completed"}
	quotaErr := &localQuotaError{model: "claude-sonnet-5", settingsURL: "https://example.com/billing"}

	tests := []struct {
		name string
		err  error
		code string
	}{
		{"canceled", context.Canceled, "canceled"},
		{"wrapped canceled", fmt.Errorf("agent run: %w", context.Canceled), "canceled"},
		{"deadline exceeded", context.DeadlineExceeded, "deadline_exceeded"},
		{"wrapped deadline exceeded", fmt.Errorf("mcp auth: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{"session not found", session.ErrNotFound, "session_not_found"},
		{"wrapped session not found", fmt.Errorf("get session: %w", session.ErrNotFound), "session_not_found"},
		{"agent not initialized", ErrAgentNotInitialized, "agent_not_initialized"},
		{"server unreachable", ErrServerUnreachable, "server_unreachable"},
		{"workspace gone", ErrWorkspaceGone, "workspace_gone"},
		{"stream closed", ErrStreamClosed, "stream_closed"},
		{"threads not supported", ErrThreadsNotSupported, "threads_not_supported"},
		{"tasks not supported", ErrTasksNotSupported, "tasks_not_supported"},
		{"discovery disabled", ErrDiscoveryDisabled, "discovery_disabled"},
		{"read-only, no reason", &ErrReadOnlyOperation{Operation: "DeleteSession"}, "read_only"},
		{"read-only, with reason and wrapped", fmt.Errorf("blocked: %w", roErr), "read_only"},
		{"provider quota", quotaErr, "provider_quota"},
		{"wrapped provider quota", fmt.Errorf("run failed: %w", quotaErr), "provider_quota"},
		{"unrecognized/internal", errors.New("disk full"), "internal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			encoded := EncodeError(tt.err)
			require.NotNil(t, encoded)
			require.Equal(t, tt.code, encoded.Code)
			require.Equal(t, tt.err.Error(), encoded.Message)

			decoded := DecodeError(encoded)
			require.Equal(t, tt.err.Error(), decoded.Error())

			switch tt.code {
			case "read_only":
				var got *ErrReadOnlyOperation
				require.True(t, errors.As(decoded, &got))
				require.True(t, IsReadOnlyError(decoded))
				var want *ErrReadOnlyOperation
				require.True(t, errors.As(tt.err, &want))
				require.Equal(t, want.Operation, got.Operation)
				require.Equal(t, want.Reason, got.Reason)
			case "provider_quota":
				gotInfo, ok := GetProviderQuotaInfo(decoded)
				require.True(t, ok)
				wantInfo, ok := GetProviderQuotaInfo(tt.err)
				require.True(t, ok)
				require.Equal(t, wantInfo, gotInfo)
			case "internal":
				// No sentinel to recover: errors.Is against the original
				// value legitimately fails post round-trip (a fresh
				// errors.New), which is exactly what "internal" promises.
				require.False(t, errors.Is(decoded, tt.err))
			default:
				sentinel, ok := sentinelCodeMap[tt.code]
				require.True(t, ok, "code %q has no registered sentinel", tt.code)
				require.True(t, errors.Is(decoded, sentinel))
				require.True(t, errors.Is(tt.err, sentinel), "sanity: original must also satisfy errors.Is")
			}

			// The *wireerr.Error itself must survive a JSON round trip
			// unchanged — this is the entire reason it exists.
			data, err := json.Marshal(encoded)
			require.NoError(t, err)
			var roundTripped wireerr.Error
			require.NoError(t, json.Unmarshal(data, &roundTripped))
			require.Equal(t, *encoded, roundTripped)
		})
	}
}

// TestEncodeDecodeError_NilRoundTrips checks the nil/nil edges EncodeError
// and DecodeError both promise.
func TestEncodeDecodeError_NilRoundTrips(t *testing.T) {
	t.Parallel()

	require.Nil(t, EncodeError(nil))
	require.Nil(t, DecodeError(nil))
}

// TestWireErrError_DoesNotImplementError pins the design choice that
// closes the typed-nil trap at the type system rather than by convention:
// *wireerr.Error has no Error() string method (see Text() instead), so it
// cannot be assigned or returned as a plain `error` at all — the compiler
// catches every such site instead of relying on someone auditing them (see
// PR 0.2c review round 1, finding 1). If this ever regains an Error()
// method, this test starts failing instead of the trap silently
// reopening.
func TestWireErrError_DoesNotImplementError(t *testing.T) {
	t.Parallel()

	_, ok := any((*wireerr.Error)(nil)).(error)
	require.False(t, ok, "*wireerr.Error must not implement error — see Text() and workspace.DecodeError")
}

// TestDecodeError_NilFieldDecodesToNilError is what a DTO field's nil case
// must do once routed through DecodeError, the only sanctioned way to turn
// one of these fields back into a plain `error`.
func TestDecodeError_NilFieldDecodesToNilError(t *testing.T) {
	t.Parallel()

	var encoded *wireerr.Error // simulates a DTO field that carries no error
	require.Nil(t, encoded)

	decoded := DecodeError(encoded)
	require.Nil(t, decoded)
}

// TestEveryWorkspaceSentinelHasACode walks every non-test .go file in this
// package looking for exported package-level `Err*` vars — the shape every
// sentinel in workspace.go's var block takes — and fails if one is not
// registered in sentinelCodes. This is what stops a new sentinel from
// silently crossing the wire as "internal" (message preserved, identity
// lost) when it should carry its own code.
func TestEveryWorkspaceSentinelHasACode(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	require.NoError(t, err)

	knownUncoded := map[string]bool{
		// ErrReadOnlyOperation is a *type*, not a sentinel var; its
		// instances are recognized structurally via errors.As and carry
		// the "read_only" code (see EncodeError). Not a gap.
	}

	var found []string
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ParseComments)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, n := range vs.Names {
					if strings.HasPrefix(n.Name, "Err") && n.IsExported() {
						found = append(found, n.Name)
					}
				}
			}
		}
	}
	require.NotEmpty(t, found, "sanity: this scan should find at least the sentinels this test knows about")

	covered := make(map[string]bool, len(sentinelCodes))
	for _, sc := range sentinelCodes {
		covered[errorVarName(sc.sentinel)] = true
	}

	for _, name := range found {
		if knownUncoded[name] {
			continue
		}
		require.True(t, covered[name], "exported sentinel %s has no wireerr code registered in sentinelCodes (internal/workspace/wireerr.go)", name)
	}
}

// sentinelVarNames maps a sentinel error value back to the package-level
// var name TestEveryWorkspaceSentinelHasACode found it under, by identity
// against the known sentinels this package declares. context.Canceled and
// context.DeadlineExceeded and session.ErrNotFound live outside this
// package and are deliberately not named here — the scan only walks this
// package's own files, so it never finds them as candidates in the first
// place. A map keyed by the error values themselves (not a switch, which
// errorlint flags as fragile against wrapped errors — moot for exact
// sentinel identity, but the map reads just as well) avoids that lint.
var sentinelVarNames = map[error]string{
	ErrAgentNotInitialized:   "ErrAgentNotInitialized",
	ErrServerUnreachable:     "ErrServerUnreachable",
	ErrWorkspaceGone:         "ErrWorkspaceGone",
	ErrStreamClosed:          "ErrStreamClosed",
	ErrThreadsNotSupported:   "ErrThreadsNotSupported",
	ErrTasksNotSupported:     "ErrTasksNotSupported",
	ErrDiscoveryDisabled:     "ErrDiscoveryDisabled",
	ErrAttachFileMissing:     "ErrAttachFileMissing",
	ErrAttachIsDirectory:     "ErrAttachIsDirectory",
	ErrAttachTooBig:          "ErrAttachTooBig",
	ErrAttachUnsupportedType: "ErrAttachUnsupportedType",
	ErrAttachReadFailed:      "ErrAttachReadFailed",
	ErrNoWorktreeForSession:  "ErrNoWorktreeForSession",
}

func errorVarName(sentinel error) string {
	return sentinelVarNames[sentinel]
}

// TestDTOFields_RoundTripJSONWithNonNilError proves every DTO field this
// package converted to *wireerr.Error (see workspace.go: LSPEvent.Error,
// MCPClientInfo.Error, ModelRefreshResult.Err, OAuthCompletion.ModelsError/
// ProxyError, AgentRunEvent.Err) still round-trips through JSON once it
// carries a non-nil error — the entire point of the conversion, since a
// plain `error` field could not.
func TestDTOFields_RoundTripJSONWithNonNilError(t *testing.T) {
	t.Parallel()

	someErr := EncodeError(errors.New("boom"))

	t.Run("LSPEvent", func(t *testing.T) {
		t.Parallel()
		roundTripJSON(t, LSPEvent{Type: LSPEventStateChanged, Name: "gopls", Error: someErr, DiagnosticCount: 3})
	})
	t.Run("MCPClientInfo", func(t *testing.T) {
		t.Parallel()
		// Not a full-struct round trip like the other DTOs below:
		// proto.MCPState implements MarshalText but not
		// UnmarshalText/UnmarshalJSON, a pre-existing asymmetry unrelated
		// to this DTO's Error field (it fires even at State's zero value),
		// which would fail json.Unmarshal regardless of what wireerr does.
		// Error is what this test is about, so it is checked directly.
		v := MCPClientInfo{Name: "github", Error: someErr}
		data, err := json.Marshal(v)
		require.NoError(t, err)
		var got struct {
			Error *wireerr.Error
		}
		require.NoError(t, json.Unmarshal(data, &got))
		require.Equal(t, someErr, got.Error)
	})
	t.Run("ModelRefreshResult", func(t *testing.T) {
		t.Parallel()
		roundTripJSON(t, ModelRefreshResult{ID: "custom", Err: someErr})
	})
	t.Run("OAuthCompletion", func(t *testing.T) {
		t.Parallel()
		roundTripJSON(t, OAuthCompletion{ModelsError: someErr, ProxyError: someErr})
	})
	t.Run("AgentRunEvent", func(t *testing.T) {
		t.Parallel()
		roundTripJSON(t, AgentRunEvent{Done: true, Err: someErr})
	})
}

func roundTripJSON[T any](t *testing.T, v T) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var got T
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, v, got)
}
