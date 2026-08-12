package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/client"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/stretchr/testify/require"
)

func newThreadsProbeClient(t *testing.T, handler http.HandlerFunc) *client.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", u.Host)
	require.NoError(t, err)
	return c
}

func TestClientWorkspaceInitializeThreadsCapabilityOldServerSupported(t *testing.T) {
	var calls atomic.Int64
	c := newThreadsProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasSuffix(r.URL.Path, "/threads"))
		calls.Add(1)
		require.NoError(t, json.NewEncoder(w).Encode([]proto.Thread{}))
	})
	ws := NewClientWorkspace(c, proto.Workspace{ID: "ws"})

	require.NoError(t, ws.InitializeThreadsCapability(t.Context()))
	require.True(t, ws.SupportsThreads())
	require.NoError(t, ws.InitializeThreadsCapability(t.Context()))
	require.EqualValues(t, 1, calls.Load())
}

func TestClientWorkspaceInitializeThreadsCapabilityOldServerUnsupported(t *testing.T) {
	var calls atomic.Int64
	c := newThreadsProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasSuffix(r.URL.Path, "/threads"))
		calls.Add(1)
		w.WriteHeader(http.StatusConflict)
	})
	ws := NewClientWorkspace(c, proto.Workspace{ID: "ws"})

	require.NoError(t, ws.InitializeThreadsCapability(t.Context()))
	require.False(t, ws.SupportsThreads())
	require.NoError(t, ws.InitializeThreadsCapability(t.Context()))
	require.EqualValues(t, 1, calls.Load())
}

func TestClientWorkspaceInitializeThreadsCapabilityReturnsTransportError(t *testing.T) {
	c := newThreadsProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasSuffix(r.URL.Path, "/threads"))
		hj, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, err := hj.Hijack()
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})
	ws := NewClientWorkspace(c, proto.Workspace{ID: "ws"})

	err := ws.InitializeThreadsCapability(t.Context())
	require.Error(t, err)
	require.False(t, ws.SupportsThreads())
	require.ErrorIs(t, ws.InitializeThreadsCapability(t.Context()), err)
}

func TestClientWorkspaceThreadsProbeIsBoundedToOneRequest(t *testing.T) {
	var calls atomic.Int64
	c := newThreadsProbeClient(t, func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/threads"):
			calls.Add(1)
			require.NoError(t, json.NewEncoder(rw).Encode([]proto.Thread{}))
		case strings.HasSuffix(r.URL.Path, "/events"):
			<-r.Context().Done()
		}
	})
	ws := NewClientWorkspace(c, proto.Workspace{ID: "ws"})
	go ws.runSubscription(func(tea.Msg) {})
	require.Eventually(t, func() bool { return ws.SupportsThreads() }, time.Second, time.Millisecond)
	require.EqualValues(t, 1, calls.Load())
	ws.Shutdown()
}

func TestClientWorkspaceShutdownCancelsAndWaitsForThreadsProbe(t *testing.T) {
	probeStarted, probeCancelled := make(chan struct{}), make(chan struct{})
	c := newThreadsProbeClient(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/threads") {
			close(probeStarted)
			<-r.Context().Done()
			close(probeCancelled)
		}
	})
	ws := NewClientWorkspace(c, proto.Workspace{ID: "ws"})
	ws.startThreadsProbe()
	<-probeStarted
	ws.Shutdown()
	select {
	case <-probeCancelled:
	case <-time.After(time.Second):
		t.Fatal("Shutdown returned before cancelling and joining the probe")
	}
}

func TestClientWorkspaceShutdownCancelsAndWaitsForCapabilityInitialization(t *testing.T) {
	probeStarted, probeCancelled := make(chan struct{}), make(chan struct{})
	c := newThreadsProbeClient(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/threads") {
			close(probeStarted)
			<-r.Context().Done()
			close(probeCancelled)
		}
	})
	ws := NewClientWorkspace(c, proto.Workspace{ID: "ws"})
	initializeDone := make(chan error, 1)
	go func() { initializeDone <- ws.InitializeThreadsCapability(t.Context()) }()
	<-probeStarted

	ws.Shutdown()
	select {
	case <-probeCancelled:
	case <-time.After(time.Second):
		t.Fatal("Shutdown returned before cancelling capability initialization")
	}
	require.ErrorIs(t, <-initializeDone, context.Canceled)
	require.ErrorIs(t, ws.InitializeThreadsCapability(t.Context()), context.Canceled)
}

func TestClientWorkspaceInitializeThreadsCapabilityDiscardsStaleResult(t *testing.T) {
	oldStarted, releaseOld := make(chan struct{}), make(chan struct{})
	c := newThreadsProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/old/"):
			close(oldStarted)
			<-releaseOld
		case strings.Contains(r.URL.Path, "/new/"):
			w.WriteHeader(http.StatusConflict)
		}
	})
	ws := NewClientWorkspace(c, proto.Workspace{ID: "old"})
	done := make(chan error, 1)
	go func() { done <- ws.InitializeThreadsCapability(t.Context()) }()
	<-oldStarted

	ws.mu.Lock()
	ws.ws.ID = "new"
	ws.threadsProbeGeneration++
	ws.threadsProbeComplete = false
	ws.threadsProbeErr = nil
	ws.setThreadsSupported(nil)
	ws.mu.Unlock()
	close(releaseOld)

	require.NoError(t, <-done)
	require.False(t, ws.SupportsThreads())
}

func TestClientWorkspaceThreadsProbeRestartsAfterStaleResult(t *testing.T) {
	oldStarted, releaseOld := make(chan struct{}), make(chan struct{})
	newStarted := make(chan struct{})
	c := newThreadsProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/old/"):
			close(oldStarted)
			<-releaseOld
		case strings.Contains(r.URL.Path, "/new/"):
			close(newStarted)
			require.NoError(t, json.NewEncoder(w).Encode([]proto.Thread{}))
		}
	})
	ws := NewClientWorkspace(c, proto.Workspace{ID: "old"})
	ws.startThreadsProbe()
	<-oldStarted

	ws.mu.Lock()
	ws.ws.ID = "new"
	ws.threadsProbeGeneration++
	ws.threadsProbeComplete = false
	ws.threadsProbeErr = nil
	ws.setThreadsSupported(nil)
	ws.mu.Unlock()
	ws.startThreadsProbe()
	close(releaseOld)

	select {
	case <-newStarted:
	case <-time.After(time.Second):
		t.Fatal("current workspace probe did not start after stale probe completed")
	}
	require.Eventually(t, ws.SupportsThreads, time.Second, time.Millisecond)
	ws.Shutdown()
}
