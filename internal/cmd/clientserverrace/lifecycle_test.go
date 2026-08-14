package clientserverrace_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/client"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestClientServerThreadLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping unix socket lifecycle test on windows")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("skipping: git not available on PATH")
	}

	provider := lifecycleProvider(t)
	defer provider.Close()

	repoRoot := repoRootFromTest(t)
	bin := buildBraidBinary(t, repoRoot)
	runDir, err := os.MkdirTemp("/tmp", "braid-lifecycle-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(runDir)) })

	socketPath := filepath.Join(runDir, "s")
	repo := filepath.Join(runDir, "repo")
	initLifecycleRepo(t, repo, provider.URL+"/v1")

	for _, dir := range []string{"home", "cache", "config", "data"} {
		require.NoError(t, os.MkdirAll(filepath.Join(runDir, dir), 0o700))
	}
	env := lifecycleEnv(runDir)

	serverCtx, stopServer := context.WithCancel(context.Background())
	serverCmd := exec.CommandContext(serverCtx, bin, "server", "--host", "unix://"+socketPath)
	serverCmd.Env = env
	require.NoError(t, serverCmd.Start())

	serverDone := make(chan struct{})
	var serverErr error
	go func() {
		serverErr = serverCmd.Wait()
		close(serverDone)
	}()
	t.Cleanup(func() {
		stopServer()
		select {
		case <-serverDone:
		case <-time.After(10 * time.Second):
			t.Error("server did not exit during cleanup")
		}
	})

	require.Eventually(t, func() bool { return pingHealth(socketPath) == nil }, 10*time.Second, 25*time.Millisecond)

	c, err := client.NewClient(repo, "unix", socketPath)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	parent, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: filepath.Join(runDir, "workspace-data")})
	require.NoError(t, err)

	created, err := c.CreateThread(ctx, parent.ID, proto.CreateThreadRequest{Name: "dogfood", Goal: "verify lifecycle", MergePolicy: "manual"})
	require.NoError(t, err)
	require.NotEmpty(t, created.WorkspaceID)

	threadClient := c.WithClientID("00000000-0000-0000-0000-000000000001")
	require.Eventually(t, func() bool {
		threadState, err := c.GetThread(ctx, parent.ID, created.ID)
		return err == nil && threadState.Status == "completed" && threadState.WorkspaceID == ""
	}, 10*time.Second, 25*time.Millisecond)
	_, err = threadClient.GetWorkspace(ctx, created.WorkspaceID)
	require.ErrorIs(t, err, client.ErrNotFound)

	require.NoError(t, c.DeleteWorkspace(ctx, parent.ID))
	select {
	case <-serverDone:
		require.NoError(t, serverErr)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not exit after the workspace was released")
	}
}

func lifecycleProvider(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, fmt.Sprintf("unexpected provider request: %s %s", r.Method, r.URL.Path), http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"dogfood\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"dogfood\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	return server
}

func lifecycleEnv(runDir string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(runDir, "home"),
		"XDG_CACHE_HOME=" + filepath.Join(runDir, "cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(runDir, "config"),
		"XDG_DATA_HOME=" + filepath.Join(runDir, "data"),
		"BRAID_DISABLE_PROVIDER_AUTO_UPDATE=1",
		"BRAID_SERVER_IDLE_TIMEOUT=0",
		"BRAID_SERVER_DETACH_GRACE=0",
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"ALL_PROXY=",
		"NO_PROXY=*",
	}
}

func initLifecycleRepo(t *testing.T, dir, providerURL string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("dogfood\n"), 0o600))
	config := fmt.Sprintf(`{"providers":{"test-provider":{"id":"test-provider","type":"openai","base_url":%q,"api_key":"test-key","discover_models":false,"models":[{"id":"test-model","context_window":4096,"default_max_tokens":100}]}},"models":{"large":{"provider":"test-provider","model":"test-model"},"small":{"provider":"test-provider","model":"test-model"}}}`, providerURL)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".braid.json"), []byte(config), 0o600))
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}, {"add", "."}, {"commit", "-m", "initial"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}
