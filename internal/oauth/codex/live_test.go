package codex

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLiveAuthAndModels exercises the real Codex backend end to end with
// whatever login is on disk: refresh the token, then read the model list.
//
// It is skipped unless CODEX_LIVE=1, because it needs a signed-in ChatGPT
// account and the network. It exists because the parts it covers cannot be
// learned from a fixture — the endpoint rejects a model-list request with no
// client_version, for instance — so this is how to re-check the contract
// after a Codex release moves something.
//
// CODEX_PROXY routes the calls when the endpoint is only reachable through
// a proxy.
//
//	CODEX_LIVE=1 go test ./internal/oauth/codex/ -run TestLive -v
func TestLiveAuthAndModels(t *testing.T) {
	if os.Getenv("CODEX_LIVE") != "1" {
		t.Skip("set CODEX_LIVE=1 to run against the real Codex backend")
	}
	ctx := context.Background()
	proxy := os.Getenv("CODEX_PROXY")

	disk, ok := TokensFromDisk()
	require.True(t, ok, "no Codex CLI login on disk to test with")

	// Prefer the token the CLI already holds. Refreshing here would spend
	// its single-use refresh token and log it out — a test run should not
	// cost the developer their Codex session.
	token, ok := disk.Token()
	if !ok {
		var err error
		token, err = RefreshToken(ctx, proxy, disk.RefreshToken)
		require.NoError(t, err)
	}
	require.NotEmpty(t, token.AccessToken)
	require.NotEmpty(t, token.RefreshToken, "a rotated-away refresh token would strand the next refresh")
	require.Positive(t, token.ExpiresIn)

	accountID := AccountID(token.AccessToken)
	require.NotEmpty(t, accountID, "the access token must name the account to bill")

	models, err := FetchModels(ctx, proxy, token.AccessToken, accountID)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	for _, model := range models {
		t.Logf("model %s (%s) ctx=%d reasoning=%v images=%v",
			model.ID, model.Name, model.ContextWindow, model.ReasoningLevels, model.SupportsImages)
		require.NotEmpty(t, model.ID)
		require.Positive(t, model.ContextWindow)
	}
}

// TestLiveFetchUsage checks the assumption FetchUsage's doc comment makes:
// that /models carries the same X-Codex-* rate-limit headers /responses is
// confirmed to send. That is unverified against the real backend, so this
// test only logs what it finds and fails solely on a request error — never
// on ok being false — since the point is to observe the current truth, not
// lock in a guess as a passing contract.
//
//	CODEX_LIVE=1 go test ./internal/oauth/codex/ -run TestLiveFetchUsage -v
func TestLiveFetchUsage(t *testing.T) {
	if os.Getenv("CODEX_LIVE") != "1" {
		t.Skip("set CODEX_LIVE=1 to run against the real Codex backend")
	}
	ctx := context.Background()
	proxy := os.Getenv("CODEX_PROXY")

	disk, ok := TokensFromDisk()
	require.True(t, ok, "no Codex CLI login on disk to test with")

	token, ok := disk.Token()
	if !ok {
		var err error
		token, err = RefreshToken(ctx, proxy, disk.RefreshToken)
		require.NoError(t, err)
	}
	require.NotEmpty(t, token.AccessToken)

	accountID := AccountID(token.AccessToken)
	require.NotEmpty(t, accountID, "the access token must name the account to bill")

	usage, ok, err := FetchUsage(ctx, proxy, token.AccessToken, accountID)
	require.NoError(t, err)
	if !ok {
		t.Log("GET /models did not carry X-Codex-* usage headers: FetchUsage's doc comment's assumption does not hold")
		return
	}
	t.Logf("GET /models carried usage headers: plan=%s primary=%d%% (window=%dm) secondary=%d%% (window=%dm)",
		usage.Plan, usage.Primary.UsedPercent, usage.Primary.WindowMinutes,
		usage.Secondary.UsedPercent, usage.Secondary.WindowMinutes)
}
