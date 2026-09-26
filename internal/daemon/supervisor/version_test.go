package supervisor

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/version"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// TestClassifyVersion covers the pure decision logic checkVersion acts
// on, without needing a real server: a ProtocolVersion difference always
// wins over a BuildID difference (the two sides may not even agree on
// how to decode ShutdownRequest), and matching everything is versionOK.
func TestClassifyVersion(t *testing.T) {
	originalBuildID := version.Commit
	version.Commit = "test-build-id"
	t.Cleanup(func() { version.Commit = originalBuildID })

	cases := []struct {
		name   string
		hello  grpcws.HelloResponse
		expect versionAction
	}{
		{
			name:   "matches exactly",
			hello:  grpcws.HelloResponse{ProtocolVersion: grpcws.ProtocolVersion, BuildID: "test-build-id"},
			expect: versionOK,
		},
		{
			name:   "protocol version differs",
			hello:  grpcws.HelloResponse{ProtocolVersion: grpcws.ProtocolVersion + 1, BuildID: "test-build-id"},
			expect: versionProtocolMismatch,
		},
		{
			name:   "build id differs, protocol matches",
			hello:  grpcws.HelloResponse{ProtocolVersion: grpcws.ProtocolVersion, BuildID: "other-build-id"},
			expect: versionBuildMismatch,
		},
		{
			name:   "both differ: protocol mismatch takes precedence",
			hello:  grpcws.HelloResponse{ProtocolVersion: grpcws.ProtocolVersion + 1, BuildID: "other-build-id"},
			expect: versionProtocolMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expect, classifyVersion(tc.hello))
		})
	}
}

// TestErrProtocolMismatch_Error pins the message's shape: both version
// numbers and a restart hint, since this is what a user sees when a
// busy daemon refuses a protocol-mismatched restart.
func TestErrProtocolMismatch_Error(t *testing.T) {
	err := &ErrProtocolMismatch{ServerProtocolVersion: 1, ClientProtocolVersion: 2}
	require.Contains(t, err.Error(), "1")
	require.Contains(t, err.Error(), "2")
	require.Contains(t, err.Error(), "daemon restart")
}
