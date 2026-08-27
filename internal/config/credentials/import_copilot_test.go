package credentials

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
)

// writeCopilotDiskToken drops a GitHub Copilot apps.json under a fake HOME,
// the same file copilot.RefreshTokenFromDisk reads, so ImportCopilot has a
// token to import.
func writeCopilotDiskToken(t *testing.T, home, oauthToken string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "github-copilot")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := `{"github.com:Iv1.b507a08c87ecfe98":{"user":"octocat","oauth_token":"` + oauthToken + `"}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "apps.json"), []byte(content), 0o600))
}

// TestImportCopilot_BoundsTheExchange pins the fix for ImportCopilot
// running its network exchange under context.TODO(), which has no
// deadline: a hung GitHub endpoint would stall the caller (startup or
// onboarding) indefinitely. Before the fix, m.exchange's fake below would
// block on <-ctx.Done() forever since a context.TODO() never fires; with
// the fix, ImportCopilot's local timeout cancels it and the call returns.
func TestImportCopilot_BoundsTheExchange(t *testing.T) {
	importCopilotTimeout = 50 * time.Millisecond
	t.Cleanup(func() { importCopilotTimeout = 15 * time.Second })

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCopilotDiskToken(t, home, "gho_disktoken")

	configPath := filepath.Join(t.TempDir(), "sennit.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("copilot", config.ProviderConfig{ID: "copilot", Name: "GitHub Copilot"})
	store := newFakeStore(&config.Config{Providers: providers}, configPath, filepath.Join(t.TempDir(), "locks"))

	m := New(store)
	exchangeStarted := make(chan struct{})
	m.exchangeToken = func(ctx context.Context, _, _ string) (*oauth.Token, error) {
		close(exchangeStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		token, ok := m.ImportCopilot()
		require.Nil(t, token)
		require.False(t, ok)
	}()

	<-exchangeStarted
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ImportCopilot did not return after its exchange context was cancelled")
	}
}
