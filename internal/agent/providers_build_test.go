package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/stretchr/testify/require"
)

// capturedRequest snapshots the single HTTP request a build*Provider probe
// sends. None of the build*Provider functions make a network call
// themselves — they only assemble fantasy.Provider options, whose concrete
// type is unexported by the fantasy provider packages — so the only way to
// observe what actually got wired in (headers, base URL, API key
// placement) is to send one real request to a local server and inspect what
// arrived.
type capturedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// newCaptureServer starts a server that records the request it receives and
// answers 500. A generic error response is enough: probe() discards
// whatever error Generate returns, since only the recorded request matters.
func newCaptureServer(t *testing.T) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.Method = r.Method
		captured.Path = r.URL.Path
		captured.Query = r.URL.RawQuery
		captured.Header = r.Header.Clone()
		captured.Body = body
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server, captured
}

// probe drives provider far enough to send one HTTP request, discarding the
// (always non-nil, since the capture server answers 500) error.
func probe(t *testing.T, provider fantasy.Provider, modelID string) {
	t.Helper()
	lm, err := provider.LanguageModel(context.Background(), modelID)
	require.NoError(t, err)
	_, _ = lm.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
}

// TestBuildAnthropicProvider covers buildAnthropicProvider's three key
// placements (plain X-Api-Key, Bearer-prefixed Authorization, MiniMax's
// forced Bearer wrapping) plus header merge and proxy error propagation.
//
// The Bearer and MiniMax cases used to clear $ANTHROPIC_API_KEY globally
// with os.Setenv to stop the SDK's own DefaultClientOptions from also
// sending X-Api-Key from the ambient env — a fix that corrupted the key for
// every other provider built afterwards and every subprocess Sennit spawns.
// The fix now strips X-Api-Key at the transport (stripHeaderTransport in
// providers.go) instead, so these subtests set ANTHROPIC_API_KEY with
// t.Setenv and assert it is untouched after building the provider — that
// is the regression this test pins.
//
//nolint:tparallel // subtests use t.Setenv, which forbids a parallel ancestor.
func TestBuildAnthropicProvider(t *testing.T) {
	t.Run("plain API key uses X-Api-Key, not Authorization", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildAnthropicProvider(server.URL, "akey", map[string]string{}, "some-provider", "", false)
		require.NoError(t, err)
		probe(t, provider, "claude-3")
		require.Equal(t, "akey", captured.Header.Get("X-Api-Key"))
		require.Empty(t, captured.Header.Get("Authorization"))
	})

	t.Run("Bearer-prefixed key goes through Authorization without X-Api-Key, and does not touch the env", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "pre-existing")
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildAnthropicProvider(server.URL, "Bearer akey", map[string]string{}, "some-provider", "", false)
		require.NoError(t, err)
		require.Equal(t, "pre-existing", os.Getenv("ANTHROPIC_API_KEY"), "the fix must not mutate the process environment")
		probe(t, provider, "claude-3")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
		require.Empty(t, captured.Header.Get("X-Api-Key"))
	})

	t.Run("MiniMax wraps the key in Bearer, strips X-Api-Key, and does not touch the env", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "pre-existing")
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildAnthropicProvider(server.URL, "akey", map[string]string{}, string(catwalk.InferenceProviderMiniMax), "", false)
		require.NoError(t, err)
		require.Equal(t, "pre-existing", os.Getenv("ANTHROPIC_API_KEY"))
		probe(t, provider, "abab6.5")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
		require.Empty(t, captured.Header.Get("X-Api-Key"))
	})

	t.Run("MiniMax China also forces the Bearer wrapping", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "pre-existing")
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildAnthropicProvider(server.URL, "akey", map[string]string{}, string(catwalk.InferenceProviderMiniMaxChina), "", false)
		require.NoError(t, err)
		probe(t, provider, "abab6.5")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
	})

	t.Run("a later Anthropic provider built in the same process still sees the ambient env key", func(t *testing.T) {
		// This is the regression the env-clearing workaround caused: build
		// a Bearer provider first (the case that used to os.Setenv the key
		// away), then a plain provider that relies on $ANTHROPIC_API_KEY —
		// it must still authenticate.
		t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
		c := newProxyTestCoordinator(t, false)

		_, err := c.builder.buildAnthropicProvider("http://example.test", "Bearer akey", map[string]string{}, "some-provider", "", false)
		require.NoError(t, err)

		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildAnthropicProvider(server.URL, "", map[string]string{}, "some-provider", "", false)
		require.NoError(t, err)
		probe(t, provider, "claude-3")
		require.Equal(t, "ambient-key", captured.Header.Get("X-Api-Key"))
	})

	t.Run("extra headers merge into the request", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildAnthropicProvider(server.URL, "akey", map[string]string{"X-Custom": "v"}, "some-provider", "", false)
		require.NoError(t, err)
		probe(t, provider, "claude-3")
		require.Equal(t, "v", captured.Header.Get("X-Custom"))
	})

	t.Run("invalid proxy URL surfaces an error and no provider", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildAnthropicProvider("", "akey", map[string]string{}, "some-provider", "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})
}

// TestBuildOpenaiProvider covers plain Bearer auth, base URL/header
// wiring, and the Codex-only usage-transport wrapping (which must not
// change the request the server sees).
func TestBuildOpenaiProvider(t *testing.T) {
	t.Parallel()

	t.Run("plain provider sends Bearer auth to the configured base URL", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildOpenaiProvider(server.URL, "akey", map[string]string{"X-Custom": "v"}, "openai", "", false)
		require.NoError(t, err)
		probe(t, provider, "gpt-4")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
		require.Equal(t, "v", captured.Header.Get("X-Custom"))
	})

	t.Run("Codex provider ID wraps the transport without changing the request", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildOpenaiProvider(server.URL, "akey", map[string]string{}, codex.ProviderID, "", false)
		require.NoError(t, err)
		probe(t, provider, "gpt-5")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
	})

	t.Run("invalid proxy URL surfaces an error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildOpenaiProvider("", "akey", map[string]string{}, "openai", "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})
}

// TestBuildOpenaiCompatProvider extends the existing 52.4%-covered path
// with the Copilot-specific transport (X-Initiator header) and the ExtraBody
// pass-through via WithSDKOptions.
func TestBuildOpenaiCompatProvider(t *testing.T) {
	t.Parallel()

	t.Run("plain provider sends Bearer auth", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildOpenaiCompatProvider(server.URL, "akey", map[string]string{}, nil, "some-vendor", false, "", false)
		require.NoError(t, err)
		probe(t, provider, "some-model")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
	})

	t.Run("extra body fields are injected into the request", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		extraBody := map[string]any{"tool_stream": true}
		provider, err := c.builder.buildOpenaiCompatProvider(server.URL, "akey", map[string]string{}, extraBody, "some-vendor", false, "", false)
		require.NoError(t, err)
		probe(t, provider, "some-model")
		var body map[string]any
		require.NoError(t, json.Unmarshal(captured.Body, &body))
		require.Equal(t, true, body["tool_stream"])
	})

	t.Run("Copilot provider ID adds the X-Initiator header via its own transport", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildOpenaiCompatProvider(server.URL, "akey", map[string]string{}, nil, string(catwalk.InferenceProviderCopilot), false, "", false)
		require.NoError(t, err)
		probe(t, provider, "gpt-4o")
		require.NotEmpty(t, captured.Header.Get("X-Initiator"))
	})

	t.Run("invalid proxy URL surfaces an error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildOpenaiCompatProvider("http://example.test", "akey", map[string]string{}, nil, "some-vendor", false, "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})
}

// TestBuildAzureProvider covers the API key header, the optional
// api_version passthrough, and header merging.
func TestBuildAzureProvider(t *testing.T) {
	t.Parallel()

	t.Run("API key goes through Api-Key, not Authorization", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildAzureProvider(server.URL, "akey", map[string]string{"X-Custom": "v"}, nil, "", false)
		require.NoError(t, err)
		probe(t, provider, "gpt-4o")
		require.Equal(t, "akey", captured.Header.Get("Api-Key"))
		require.Empty(t, captured.Header.Get("Authorization"))
		require.Equal(t, "v", captured.Header.Get("X-Custom"))
	})

	t.Run("a configured apiVersion reaches the request", func(t *testing.T) {
		t.Parallel()
		// fantasy's azure.WithAPIVersion stores the value but never
		// applies it to the request (see
		// charm.land/fantasy/providers/azure.New, which never reads
		// o.apiVersion back out — confirmed against both our pinned
		// v0.40.0 and the newest published v0.41.2) — an upstream gap,
		// not ours to fix here. buildAzureProvider works around it by
		// adding the query parameter itself via azureAPIVersionTransport,
		// so this asserts the value actually lands on the wire rather
		// than merely that construction does not error.
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildAzureProvider(server.URL, "akey", map[string]string{}, map[string]string{"apiVersion": "2024-05-05"}, "", false)
		require.NoError(t, err)
		probe(t, provider, "gpt-4o")
		values, err := url.ParseQuery(captured.Query)
		require.NoError(t, err)
		require.Equal(t, "2024-05-05", values.Get("api-version"))
	})

	t.Run("invalid proxy URL surfaces an error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildAzureProvider("http://example.test", "akey", map[string]string{}, nil, "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})
}

// TestBuildGoogleProvider covers the Gemini API key header and header
// merging.
func TestBuildGoogleProvider(t *testing.T) {
	t.Parallel()

	t.Run("API key goes through X-Goog-Api-Key", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		provider, err := c.builder.buildGoogleProvider(server.URL, "akey", map[string]string{"X-Custom": "v"}, "", false)
		require.NoError(t, err)
		probe(t, provider, "gemini-2.5-pro")
		require.Equal(t, "akey", captured.Header.Get("X-Goog-Api-Key"))
		require.Equal(t, "v", captured.Header.Get("X-Custom"))
	})

	t.Run("invalid proxy URL surfaces an error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildGoogleProvider("http://example.test", "akey", map[string]string{}, "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})
}

// TestBuildOpenrouterAndVercelProvider: neither buildOpenrouterProvider nor
// buildVercelProvider takes a base URL (both silently ignore the baseURL
// argument passed at their call sites in buildProvider — the underlying
// fantasy providers always target their real hosted API), and neither
// coordinator method exposes a way to inject a custom RoundTripper directly.
// Redirecting the request they'd send to a local server would require
// buildProviderHTTPClient's proxyURL to route through a real HTTP CONNECT
// tunnel (for the https default endpoint), which is out of scope to fake up
// here. So these two are covered at construction only: every branch in the
// function runs (API key option, optional headers, optional HTTP client,
// proxy error), just without ever calling Generate — no network happens.
func TestBuildOpenrouterAndVercelProvider(t *testing.T) {
	t.Parallel()

	t.Run("openrouter: construction wires all options without error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildOpenrouterProvider("ignored-base-url", "akey", map[string]string{"X-Custom": "v"}, "", false)
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("openrouter: invalid proxy URL surfaces an error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildOpenrouterProvider("", "akey", map[string]string{}, "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})

	t.Run("vercel: construction wires all options without error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildVercelProvider("ignored-base-url", "akey", map[string]string{"X-Custom": "v"}, "", false)
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("vercel: invalid proxy URL surfaces an error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildVercelProvider("", "akey", map[string]string{}, "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})
}

// TestBuildBedrockProvider covers apiKey precedence (explicit apiKey over
// the AWS_BEARER_TOKEN_BEDROCK env fallback over the SDK's own credential
// chain) and header merging at construction. bedrock.New itself makes no
// network call, so all of this runs offline; what it produces internally
// (region, credential source) is only resolved lazily inside
// LanguageModel/Generate, which would need either a real or a
// CONNECT-capable fake AWS endpoint to observe safely, so this test does
// not go that far — see TestBuildOpenrouterAndVercelProvider for the same
// tradeoff.
//
// Two subtests use t.Setenv on AWS_BEARER_TOKEN_BEDROCK, which forbids a
// parallel ancestor, so this test itself does not call t.Parallel().
//
//nolint:tparallel // subtests use t.Setenv, which forbids a parallel ancestor.
func TestBuildBedrockProvider(t *testing.T) {
	t.Run("explicit API key builds without error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildBedrockProvider("akey", map[string]string{"X-Custom": "v"}, "some-provider", "", false)
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("AWS_BEARER_TOKEN_BEDROCK env fallback builds without error", func(t *testing.T) {
		t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "env-token")
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildBedrockProvider("", map[string]string{}, "some-provider", "", false)
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("no API key at all still builds, deferring to the SDK's credential chain", func(t *testing.T) {
		t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildBedrockProvider("", map[string]string{}, "some-provider", "", false)
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("bedrock-europe and the default both build without error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildBedrockProvider("akey", map[string]string{}, string(catwalk.InferenceProviderBedrockEurope), "", false)
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("invalid proxy URL surfaces an error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildBedrockProvider("akey", map[string]string{}, "some-provider", "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})
}

// TestBuildGoogleVertexProvider covers construction only: the vertex
// project/location and skip-auth path are resolved lazily inside
// LanguageModel via Google Application Default Credentials, which would
// either need real GCP credentials or hit the metadata server over the
// network to exercise — both out of bounds here. This still confirms
// project/location and header wiring do not error at construction.
func TestBuildGoogleVertexProvider(t *testing.T) {
	t.Parallel()

	t.Run("construction wires project/location and headers without error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildGoogleVertexProvider(map[string]string{"X-Custom": "v"}, map[string]string{"project": "proj", "location": "us-central1"}, "", false)
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("invalid proxy URL surfaces an error", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		provider, err := c.builder.buildGoogleVertexProvider(map[string]string{}, nil, "://bad", false)
		require.Error(t, err)
		require.Nil(t, provider)
	})
}

// TestBuildProvider covers the top-level dispatcher: the OpenCode
// Anthropic-Messages override, the anthropic-beta header it adds for a
// thinking model, each provider-type branch, the ZAI extra_body injection,
// and the known/unknown custom-provider fallback.
func buildTestProvider(t *testing.T, c *coordinator, providerCfg config.ProviderConfig, model config.SelectedModel) (fantasy.Provider, error) {
	t.Helper()
	effective, err := providerruntime.FromConfig(providerCfg, c.cfg.Config().RuntimeResolver())
	if err != nil {
		return nil, err
	}
	if providerCfg.Type == "google-vertex" {
		effective.ExtraParams["project"] = "p"
		effective.ExtraParams["location"] = "us-central1"
	}

	// c.cfg's Config came from configruntime.Load, which freezes
	// RuntimeProviders on publish (see ConfigStore.setConfig) exactly like
	// Providers — so it can no longer be edited in place. Build a fresh,
	// unfrozen clone carrying the new runtime provider and republish that
	// through a new store before building, rather than mutating the live
	// snapshot; config.NewStore (unlike the load pipeline) never freezes
	// what it publishes.
	base := *c.cfg.Config()
	runtimeProviders := csync.NewMap[string, providerstate.Provider]()
	if base.RuntimeProviders != nil {
		for id, p := range base.RuntimeProviders.Seq2() {
			runtimeProviders.Set(id, p)
		}
	}
	base.RuntimeProviders = runtimeProviders
	base.SetRuntimeProvider(providerCfg.ID, effective)

	c.cfg = config.NewStore(config.StoreOptions{
		Config:     &base,
		WorkingDir: c.cfg.WorkingDir(),
		Resolver:   c.cfg.Resolver(),
	})

	return c.builder.buildProviderForSnapshot(providerCfg, model, false, c.builder.runtimeConfigSnapshot())
}

func TestBuildProvider(t *testing.T) {
	t.Parallel()

	t.Run("openai type routes to buildOpenaiProvider", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{Type: "openai", APIKey: "akey", BaseURL: server.URL}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "gpt-4"})
		require.NoError(t, err)
		probe(t, provider, "gpt-4")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
	})

	t.Run("anthropic type adds interleaved-thinking beta header for a thinking model", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{Type: "anthropic", APIKey: "akey", BaseURL: server.URL}
		model := config.SelectedModel{Model: "claude-3", Think: true}
		provider, err := buildTestProvider(t, c, providerCfg, model)
		require.NoError(t, err)
		probe(t, provider, "claude-3")
		require.Equal(t, "interleaved-thinking-2025-05-14", captured.Header.Get("Anthropic-Beta"))
	})

	t.Run("anthropic type appends to an existing anthropic-beta header instead of replacing it", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{
			Type: "anthropic", APIKey: "akey", BaseURL: server.URL,
			ExtraHeaders: map[string]string{"anthropic-beta": "other-beta-2024"},
		}
		model := config.SelectedModel{Model: "claude-3", Think: true}
		provider, err := buildTestProvider(t, c, providerCfg, model)
		require.NoError(t, err)
		probe(t, provider, "claude-3")
		require.Equal(t, "other-beta-2024,interleaved-thinking-2025-05-14", captured.Header.Get("Anthropic-Beta"))
	})

	t.Run("anthropic type without thinking does not add the beta header", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{Type: "anthropic", APIKey: "akey", BaseURL: server.URL}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "claude-3"})
		require.NoError(t, err)
		probe(t, provider, "claude-3")
		require.Empty(t, captured.Header.Get("Anthropic-Beta"))
	})

	t.Run("OpenCode provider ID routes its Messages-API model list to the anthropic builder", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{
			Type: "openai-compat", ID: string(catwalk.InferenceProviderOpenCodeGo),
			APIKey: "akey", BaseURL: server.URL + "/v1",
		}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "qwen3.7-max"})
		require.NoError(t, err)
		probe(t, provider, "qwen3.7-max")
		// The anthropic builder was used (X-Api-Key), not openaicompat
		// (Bearer Authorization), and the "/v1" suffix was trimmed from
		// the base URL before it got there.
		require.Equal(t, "akey", captured.Header.Get("X-Api-Key"))
		require.Equal(t, "/v1/messages", captured.Path)
	})

	t.Run("OpenCode provider ID with a non-Messages model stays on openaicompat", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{
			Type: "openai-compat", ID: string(catwalk.InferenceProviderOpenCodeGo),
			APIKey: "akey", BaseURL: server.URL,
		}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "some-other-model"})
		require.NoError(t, err)
		probe(t, provider, "some-other-model")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
	})

	t.Run("azure type routes to buildAzureProvider", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{Type: "azure", APIKey: "akey", BaseURL: server.URL}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "gpt-4o"})
		require.NoError(t, err)
		probe(t, provider, "gpt-4o")
		require.Equal(t, "akey", captured.Header.Get("Api-Key"))
	})

	t.Run("google type routes to buildGoogleProvider", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{Type: "google", APIKey: "akey", BaseURL: server.URL}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "gemini-2.5-pro"})
		require.NoError(t, err)
		probe(t, provider, "gemini-2.5-pro")
		require.Equal(t, "akey", captured.Header.Get("X-Goog-Api-Key"))
	})

	t.Run("google-vertex type routes to buildGoogleVertexProvider", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		providerCfg := config.ProviderConfig{Type: "google-vertex"}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "gemini-2.5-pro"})
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("bedrock type routes to buildBedrockProvider", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		providerCfg := config.ProviderConfig{Type: "bedrock", APIKey: "akey"}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "claude-3"})
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("openrouter type routes to buildOpenrouterProvider", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		providerCfg := config.ProviderConfig{Type: "openrouter", APIKey: "akey"}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "some-model"})
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("vercel type routes to buildVercelProvider", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		providerCfg := config.ProviderConfig{Type: "vercel", APIKey: "akey"}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "some-model"})
		require.NoError(t, err)
		require.NotNil(t, provider)
	})

	t.Run("openaicompat type with ZAI ID injects tool_stream into extra_body", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{
			Type: "openai-compat", ID: string(catwalk.InferenceProviderZAI),
			APIKey: "akey", BaseURL: server.URL,
		}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "glm-5"})
		require.NoError(t, err)
		probe(t, provider, "glm-5")
		var body map[string]any
		require.NoError(t, json.Unmarshal(captured.Body, &body))
		require.Equal(t, true, body["tool_stream"])
	})

	t.Run("openaicompat type with ZAI ID does not mutate the shared providerCfg.ExtraBody map", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, _ := newCaptureServer(t)
		// providerCfg.ExtraBody is the same map instance a *config.Config
		// hands out to every buildProvider call for this provider, and
		// possibly to concurrent callers too. Mutating it in place (the
		// old bug) both races those callers and leaks tool_stream into
		// every later provider built from the same config generation.
		sharedExtraBody := map[string]any{"other": "value"}
		providerCfg := config.ProviderConfig{
			Type: "openai-compat", ID: string(catwalk.InferenceProviderZAI),
			APIKey: "akey", BaseURL: server.URL, ExtraBody: sharedExtraBody,
		}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "glm-5"})
		require.NoError(t, err)
		probe(t, provider, "glm-5")
		require.NotContains(t, sharedExtraBody, "tool_stream", "buildProvider must not write into the caller's ExtraBody map")
		require.Equal(t, map[string]any{"other": "value"}, sharedExtraBody)
	})

	t.Run("openaicompat type without a special vendor ID leaves extra_body untouched", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{Type: "openai-compat", APIKey: "akey", BaseURL: server.URL}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "some-model"})
		require.NoError(t, err)
		probe(t, provider, "some-model")
		var body map[string]any
		require.NoError(t, json.Unmarshal(captured.Body, &body))
		require.NotContains(t, body, "tool_stream")
	})

	t.Run("a known custom provider type falls back to openaicompat", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		server, captured := newCaptureServer(t)
		providerCfg := config.ProviderConfig{Type: "ollama", APIKey: "akey", BaseURL: server.URL}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "llama3"})
		require.NoError(t, err)
		probe(t, provider, "llama3")
		require.Equal(t, "Bearer akey", captured.Header.Get("Authorization"))
	})

	t.Run("an unknown provider type is rejected", func(t *testing.T) {
		t.Parallel()
		c := newProxyTestCoordinator(t, false)
		providerCfg := config.ProviderConfig{Type: "totally-unknown"}
		provider, err := buildTestProvider(t, c, providerCfg, config.SelectedModel{Model: "m"})
		require.Error(t, err)
		require.Nil(t, provider)
		require.Contains(t, err.Error(), "totally-unknown")
	})
}
