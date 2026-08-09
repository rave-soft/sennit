package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/rave-soft/braid/internal/client"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/stretchr/testify/require"
)

// TestCreateWorkspaceOnLiveServer_RetriesPastExitingServer covers the
// startup race left over from making the shutdown decision final: a client
// can reach a server in the instant between its committing to an idle
// shutdown and its socket disappearing. Failing the CLI there would be a
// regression, so the client has to bring up a replacement and ask again.
func TestCreateWorkspaceOnLiveServer_RetriesPastExitingServer(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	replaced := 0
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/workspaces", r.URL.Path)
		mu.Lock()
		creates++
		// Only once the client has stood up a replacement does the create
		// succeed, so passing this test requires the retry to be real.
		serve := replaced > 0
		mu.Unlock()
		if !serve {
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(proto.Workspace{ID: "ws-new"}))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	c, err := client.NewClient(t.TempDir(), "tcp", u.Host)
	require.NoError(t, err)

	ws, err := createWorkspaceOnLiveServer(t.Context(), c, proto.Workspace{Path: t.TempDir()},
		func() error {
			mu.Lock()
			replaced++
			mu.Unlock()
			return nil
		})
	require.NoError(t, err)
	require.Equal(t, "ws-new", ws.ID)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, replaced, "exactly one replacement must be spawned")
	require.Equal(t, 2, creates)
}

// TestCreateWorkspaceOnLiveServer_GivesUpOnOtherFailures makes sure the
// retry is scoped to the shutdown race and does not paper over real
// errors by respawning servers.
func TestCreateWorkspaceOnLiveServer_GivesUpOnOtherFailures(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	c, err := client.NewClient(t.TempDir(), "tcp", u.Host)
	require.NoError(t, err)

	_, err = createWorkspaceOnLiveServer(t.Context(), c, proto.Workspace{Path: t.TempDir()},
		func() error {
			t.Fatal("a non-lifecycle failure must not spawn a replacement server")
			return nil
		})
	require.Error(t, err)
}
