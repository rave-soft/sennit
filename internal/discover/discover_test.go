package discover

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestDiscoverModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "model-a", "object": "model", "owned_by": "org"},
				{"id": "model-b", "object": "model", "owned_by": "org"}
			]
		}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, "model-a", models[0].ID)
	require.Equal(t, "model-b", models[1].ID)
}

// TestDiscoverModels_FiltersGGUFPaths guards against the llama.cpp
// incident: llama-server's /v1/models (ollama-shaped response) puts the
// loaded --model path verbatim into "id" instead of a real model name.
// Treating that path as a discovered model is how a provider ends up with
// a single junk entry like "/models/Qwen3.6-...gguf" replacing a real
// model list.
func TestDiscoverModels_FiltersGGUFPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "/models/Qwen3.6-30B-A3B-Instruct-2507-Q4_K_M.gguf", "object": "model"},
				{"id": "real-model", "object": "model"}
			]
		}`))
	}))
	defer server.Close()

	cfg := Config{ID: "qwen36-local", BaseURL: server.URL + "/v1", APIKey: "test-key"}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "real-model", models[0].ID)
}

// TestDiscoverModels_AllJunkIsAnError checks the case that actually broke
// qwen36-local: an endpoint whose entire /v1/models response is junk paths
// must fail loudly rather than "successfully" returning a single garbage
// model that then overwrites a real, hand-configured list.
func TestDiscoverModels_AllJunkIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "/models/Qwen3.6-30B-A3B-Instruct-2507-Q4_K_M.gguf", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{ID: "qwen36-local", BaseURL: server.URL + "/v1", APIKey: "test-key"}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.Error(t, err)
	require.Nil(t, models)
	require.Contains(t, err.Error(), "endpoint does not expose a usable model list")
}

func TestDiscoverModels_ExistingModelsWin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "model-a", "object": "model"},
				{"id": "model-b", "object": "model"}
			]
		}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		ExistingModels: []catwalk.Model{
			{ID: "model-a", Name: "My Custom Name", ContextWindow: 200000, CanReason: true},
		},
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 2)

	require.Equal(t, "model-a", models[0].ID)
	require.Equal(t, "My Custom Name", models[0].Name)
	require.Equal(t, int64(200000), models[0].ContextWindow)
	require.True(t, models[0].CanReason)

	require.Equal(t, "model-b", models[1].ID)
	require.Equal(t, "model-b", models[1].Name)
}

func TestDiscoverModels_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.Error(t, err)
	require.Nil(t, models)
}

// failingResolver fails ResolveValue for one specific value (simulating a
// failing $(cmd) substitution) and passes everything else through
// unchanged, so a test can target either base_url or api_key resolution in
// isolation.
type failingResolver struct {
	failFor string
	err     error
}

func (f *failingResolver) ResolveValue(val string) (string, error) {
	if val == f.failFor {
		return "", f.err
	}
	return val, nil
}

// TestDiscoverModels_BaseURLResolveErrorSurfaces guards against doRequest
// swallowing a base_url resolution failure: previously it discarded the
// error and built a request against the literal, unresolved template,
// which the server then answered (with whatever status an obviously
// malformed URL/host produces) instead of the real, actionable resolver
// error ever reaching the user.
func TestDiscoverModels_BaseURLResolveErrorSurfaces(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	resolveErr := errors.New("command substitution failed: exit status 1")
	cfg := Config{
		ID:      "test",
		BaseURL: "$(broken-cmd)",
		APIKey:  "test-key",
	}
	resolver := &failingResolver{failFor: "$(broken-cmd)", err: resolveErr}

	models, err := DiscoverModels(context.Background(), cfg, resolver)
	require.Error(t, err)
	require.ErrorIs(t, err, resolveErr)
	require.Nil(t, models)
	require.Zero(t, requests, "a failed base_url resolution must not fall through to an HTTP request")
}

// TestDiscoverModels_APIKeyResolveErrorSurfaces is
// TestDiscoverModels_BaseURLResolveErrorSurfaces's counterpart for
// api_key: a failing $(cmd) substitution there used to surface to the
// user as a plain 401 from the (unauthenticated) request that went out
// anyway, hiding the real cause.
func TestDiscoverModels_APIKeyResolveErrorSurfaces(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	resolveErr := errors.New("command substitution failed: exit status 1")
	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "$(broken-cmd)",
	}
	resolver := &failingResolver{failFor: "$(broken-cmd)", err: resolveErr}

	models, err := DiscoverModels(context.Background(), cfg, resolver)
	require.Error(t, err)
	require.ErrorIs(t, err, resolveErr)
	require.Nil(t, models)
	require.Zero(t, requests, "a failed api_key resolution must not fall through to an HTTP request that surfaces as a 401")
}

func TestDiscoverModels_ExtraHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "custom-value", r.Header.Get("X-Custom"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "m1", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		ExtraHeaders: map[string]string{
			"X-Custom": "custom-value",
		},
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 1)
}

type mockResolver struct{}

func (m *mockResolver) ResolveValue(val string) (string, error) { return val, nil }

type envResolver struct {
	env map[string]string
}

func (e *envResolver) ResolveValue(val string) (string, error) {
	if v, ok := e.env[val]; ok {
		return v, nil
	}
	return val, nil
}

func TestDiscoverModels_ResolvesShellVariables(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer resolved-key", r.Header.Get("Authorization"))
		require.Equal(t, "resolved-header", r.Header.Get("X-Custom"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "m1", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "$MY_API_KEY",
		ExtraHeaders: map[string]string{
			"X-Custom": "$MY_HEADER",
		},
	}

	resolver := &envResolver{env: map[string]string{
		"$MY_API_KEY": "resolved-key",
		"$MY_HEADER":  "resolved-header",
	}}

	models, err := DiscoverModels(context.Background(), cfg, resolver)
	require.NoError(t, err)
	require.Len(t, models, 1)
}

func TestDiscoverModels_SkipsEmptyExtraHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("X-Empty"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "m1", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		ExtraHeaders: map[string]string{
			"X-Empty": "$UNSET_VAR",
		},
	}

	resolver := &envResolver{env: map[string]string{
		"$UNSET_VAR": "",
	}}

	models, err := DiscoverModels(context.Background(), cfg, resolver)
	require.NoError(t, err)
	require.Len(t, models, 1)
}

func TestDiscoverModels_NoAuthWhenNoAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "m1", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "",
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 1)
}

func TestDiscoverModels_RoutesThroughProxy(t *testing.T) {
	var proxyRequestURI string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A Transport with Proxy set sends an absolute-URI request line to
		// the proxy for plain HTTP targets, so the proxy sees the full
		// target URL rather than just a path.
		proxyRequestURI = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "model-a", "object": "model"}]}`))
	}))
	defer proxy.Close()

	cfg := Config{
		ID: "test",
		// TEST-NET-1 (RFC 5737): guaranteed non-routable, so a successful
		// result proves the request went through the proxy, not directly.
		BaseURL:  "http://192.0.2.1/v1",
		ProxyURL: proxy.URL,
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "model-a", models[0].ID)
	require.Contains(t, proxyRequestURI, "192.0.2.1")
}

func TestDiscoverModels_ProxyDirectIgnoresEnvProxy(t *testing.T) {
	var proxyHit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer proxy.Close()

	var directHit bool
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "model-a", "object": "model"}]}`))
	}))
	defer direct.Close()

	// net/http caches the environment-derived proxy function process-wide
	// behind a sync.Once (see transport.go's envProxyOnce/envProxyFunc), so
	// asserting that an *empty* ProxyURL picks up a later t.Setenv'd
	// HTTP_PROXY would be flaky: whichever test in this binary first makes
	// a request through the default transport locks in that env snapshot
	// for the rest of the process. We sidestep that entirely: Transport.Proxy
	// is only ever consulted by net/http when non-nil (transport.go,
	// connectMethodForRequest), so proxyDirect's explicitly nil'd Proxy
	// field guarantees HTTP_PROXY is never even read here, deterministically
	// and regardless of caching or test order.
	t.Setenv("HTTP_PROXY", proxy.URL)

	cfg := Config{
		ID:       "test",
		BaseURL:  direct.URL + "/v1",
		ProxyURL: proxyDirect,
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.True(t, directHit, "expected the request to reach the direct target server")
	require.False(t, proxyHit, "expected the request not to reach the proxy stand-in")
}

func TestNewProxyHTTPClient_ProxyDirect(t *testing.T) {
	t.Parallel()
	client, err := newProxyHTTPClient(proxyDirect)
	require.NoError(t, err)
	require.NotNil(t, client)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok, "expected an *http.Transport")
	require.Nil(t, transport.Proxy, "Proxy must be nil'd out, not left unset, so env proxy vars are ignored")
}

func TestDiscoverModels_InvalidProxyURL(t *testing.T) {
	cfg := Config{
		ID:       "my-provider",
		BaseURL:  "http://example.com/v1",
		ProxyURL: "ftp://proxy:21",
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.Error(t, err)
	require.Nil(t, models)
	require.Contains(t, err.Error(), "my-provider")
	require.Contains(t, err.Error(), "proxy_url")
}

func TestStripV1Suffix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"http://localhost:8000/v1", "http://localhost:8000"},
		{"http://localhost:8000/v1/", "http://localhost:8000"},
		{"http://localhost:8000", "http://localhost:8000"},
		{"http://localhost:8000/", "http://localhost:8000"},
		{"http://localhost:8000/api/v1", "http://localhost:8000/api"},
		{"", ""},
	}
	for _, tt := range tests {
		got := stripV1Suffix(tt.input)
		require.Equal(t, tt.want, got, "stripV1Suffix(%q)", tt.input)
	}
}
