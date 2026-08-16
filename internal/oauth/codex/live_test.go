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
//	CODEX_LIVE=1 go test ./internal/oauth/codex/ -run TestLive -v
func TestLiveAuthAndModels(t *testing.T) {
	if os.Getenv("CODEX_LIVE") != "1" {
		t.Skip("set CODEX_LIVE=1 to run against the real Codex backend")
	}
	ctx := context.Background()

	disk, ok := TokensFromDisk()
	require.True(t, ok, "no Codex CLI login on disk to test with")

	token, err := RefreshToken(ctx, disk.RefreshToken)
	require.NoError(t, err, "refreshing the on-disk login must work")
	require.NotEmpty(t, token.AccessToken)
	require.NotEmpty(t, token.RefreshToken, "a rotated-away refresh token would strand the next refresh")
	require.Positive(t, token.ExpiresIn)

	accountID := AccountID(token.AccessToken)
	require.NotEmpty(t, accountID, "the access token must name the account to bill")

	models, err := FetchModels(ctx, token.AccessToken, accountID)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	for _, model := range models {
		t.Logf("model %s (%s) ctx=%d reasoning=%v images=%v",
			model.ID, model.Name, model.ContextWindow, model.ReasoningLevels, model.SupportsImages)
		require.NotEmpty(t, model.ID)
		require.Positive(t, model.ContextWindow)
	}
}
