package app

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestNew_InitCoderAgentFailureRollsBackStartedResources is the
// regression test for New's failure path when InitCoderAgent errors: by
// that point New has already armed MCP initialization, started the
// config/skills external-change watchers, and bridged herdr — all live
// goroutines. New used to just wrap and return the error, leaving every
// one of them running forever with no *App left for the caller to shut
// down. It must now roll all of that back before returning.
//
// A provider with no coder agent entry in cfg.Agents is enough to reach
// InitCoderAgent and have it fail (see initCoderAgent's "coder agent
// configuration is missing" check) without needing a real model or
// network access.
func TestNew_InitCoderAgentFailureRollsBackStartedResources(t *testing.T) {
	setBootstrapTestEnv(t)

	dataDir := t.TempDir()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	// Captured after db.Connect: the pooled *sql.DB's own connectionOpener
	// goroutine is not New's to roll back (New's caller owns conn - see
	// Bootstrap's own dbConnected release), so it must not count as a leak.
	ignoreBaseline := goleak.IgnoreCurrent()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("test-provider", config.ProviderConfig{ID: "test-provider"})
	cfg := &config.Config{
		Providers: providers,
		Agents:    map[string]config.Agent{}, // deliberately no AgentCoder entry
		Options:   &config.Options{DataDirectory: t.TempDir()},
	}
	store := config.NewTestStore(t, cfg, config.WithWorkingDir(t.TempDir()))
	skillsMgr := skills.NewManager(nil, nil, nil)

	a, err := New(context.Background(), conn, store, skillsMgr)
	require.Error(t, err)
	require.Nil(t, a)
	require.Contains(t, err.Error(), "coder agent")

	// The rollback's own stop/cancel/join calls are synchronous within
	// New, but goleak needs a moment to observe goroutines actually
	// exiting the runtime scheduler.
	deadline := time.Now().Add(2 * time.Second)
	var leakErr error
	for time.Now().Before(deadline) {
		leakErr = goleak.Find(ignoreBaseline)
		if leakErr == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("a failed InitCoderAgent must roll back MCP/watchers/herdr, not leak their goroutines: %v", leakErr)
}
