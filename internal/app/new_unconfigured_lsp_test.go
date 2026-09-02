package app

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

// TestNew_UnconfiguredProviderStillWiresLSPCallback is the regression test
// for New's early return on an unconfigured provider skipping the LSP
// state callback: SetCallback and TrackConfigured used to sit below the
// !cfg.IsConfigured() early return, alongside InitCoderAgent, so a
// project with no runtime provider yet never had its callback installed.
// Onboarding a provider later goes through initCoderAgent
// (internal/app/services.go), which never touches LSP wiring either, so
// the sidebar stayed dead until the app restarted. The callback must now
// be installed regardless of whether a provider is configured.
//
// A configured LSP server with no configured provider is enough to reach
// TrackConfigured and observe the callback fire, without needing a real
// model, network access, or an actual LSP binary: TrackConfigured
// announces configured-but-not-started servers via callback(name, nil),
// which updateLSPState records as StateUnstarted.
func TestNew_UnconfiguredProviderStillWiresLSPCallback(t *testing.T) {
	setBootstrapTestEnv(t)

	// Connect at config.GlobalDBDir(), the same path New's mainDBRelease
	// releases (internal/app/app.go:130-131): a successful New hands that
	// pooled connection to a.Shutdown() to release, so the test must not
	// also release it itself, nor connect at a different path than
	// Shutdown will target (see bootstrap.go's dbConnected handoff for
	// the production analogue).
	dataDir := config.GlobalDBDir()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](), // deliberately empty: unconfigured
		LSP: config.LSPs{
			"testlsp": config.LSPConfig{Command: "echo"},
		},
		Options: &config.Options{DataDirectory: t.TempDir()},
	}
	store := configtest.NewStore(t, cfg, configtest.WithWorkingDir(t.TempDir()))
	skillsMgr := skills.NewManager(nil, nil, nil)

	a, err := New(context.Background(), conn, store, skillsMgr)
	require.NoError(t, err)
	require.NotNil(t, a)
	t.Cleanup(a.Shutdown)

	require.False(t, cfg.IsConfigured(), "test setup must exercise the unconfigured-provider path")

	// TrackConfigured runs in its own goroutine, so give it a moment to
	// call back into app.lsp before asserting on the recorded state.
	require.Eventually(t, func() bool {
		info, ok := a.GetLSPState("testlsp")
		return ok && info.State == lsp.StateUnstarted
	}, 2*time.Second, 10*time.Millisecond,
		"LSP callback must be installed before New returns, even without a configured provider")
}
