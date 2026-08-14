package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/config"
	"github.com/stretchr/testify/require"
)

func TestListResourcesStaleResultDoesNotRepublishAfterTeardown(t *testing.T) {
	const name = "stale-resources"
	r := NewRegistry()
	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	old := liveSessionWithCapabilities(t, "old", "old", "res://old")
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, old)
	r.sessionOwners[name] = owner
	r.states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: old})
	r.publishMu.Unlock()
	r.ping = func(context.Context, *ClientSession, time.Duration) error { return nil }

	started := make(chan struct{})
	release := make(chan struct{})
	r.listResources = func(context.Context, *ClientSession) ([]*Resource, error) {
		close(started)
		<-release
		return []*Resource{{Name: "stale", URI: "res://stale"}}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.ListResources(context.Background(), cfg, name)
		done <- err
	}()
	<-started
	r.teardown(name)
	r.updateState(name, StateDisabled, nil, nil, Counts{})
	close(release)
	require.NoError(t, <-done)
	_, ok := r.allResources.Get(name)
	require.False(t, ok)
	info, ok := r.states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateDisabled, info.State)
	require.Zero(t, info.Counts.Resources)
}
