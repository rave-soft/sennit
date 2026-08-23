package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/rave-soft/sennit/internal/oauth/codex"
)

// Providers returns the provider catalog for cfg.
//
// It used to memoize the result process-globally via sync.Once, but that
// cached the first cfg it ever saw and kept returning that answer to every
// other *Config passed in afterwards — fatal once a single process can hold
// several ConfigStores (e.g. one per workspace) with different
// DisableDefaultProviders settings. embedded.GetAll() is just an in-memory
// slice, so recomputing per call is cheap; callers that already hold a
// ConfigStore should prefer its cached ConfigStore.KnownProviders() instead
// of calling this repeatedly.
func Providers(cfg *Config) []catwalk.Provider {
	// The provider catalog ships with the binary. Upstream refreshed it
	// from Charm's catwalk service at startup; that call is removed, so
	// Sennit never reaches the network to decide what models exist.
	// Anything the embedded list does not know about is declared in the
	// user's own config.
	if cfg.Options.DisableDefaultProviders {
		return nil
	}
	return append(embedded.GetAll(), CodexProvider())
}

// CodexProvider is the catalog entry for OpenAI Codex.
//
// Codex is not in the embedded catalog: it is not an API-key provider, it is
// what a ChatGPT subscription unlocks, so it only exists once the user signs
// in. Declaring it here rather than writing an endpoint and a type into the
// user's config at sign-in time means a later Sennit can move the endpoint
// or change the headers without every existing install being stuck on the
// values it was set up with.
//
// The model list stays empty on purpose: which models an account may use is
// per-plan and changes over time, so it is fetched from OpenAI's Codex API at
// sign-in (see oauth/codex.FetchModels) and merged in from the user's config
// (see mergeCatalogProviders).
func CodexProvider() catwalk.Provider {
	return catwalk.Provider{
		ID:          catwalk.InferenceProvider(codex.ProviderID),
		Name:        codex.ProviderName,
		APIEndpoint: codex.APIBaseURL,
		// The Codex endpoint speaks the Responses API, which is exactly
		// what the OpenAI provider type sends.
		Type: catwalk.TypeOpenAI,
	}
}

func (c *ProviderConfig) TestConnection(resolver VariableResolver) error {
	var (
		providerID = catwalk.InferenceProvider(c.ID)
		testURL    = ""
		headers    = make(map[string]string)
		apiKey, _  = resolver.ResolveValue(c.APIKey)
	)

	switch providerID {
	case catwalk.InferenceProviderMiniMax, catwalk.InferenceProviderMiniMaxChina:
		// NOTE: MiniMax has no good endpoint we can use to validate the API key.
		return nil
	case catwalk.InferenceProviderAlibabaSingapore:
		// NOTE: Alibaba has no good endpoint we can use to validate the API key.
		// Let's at least check the pattern.
		if !strings.HasPrefix(apiKey, "sk-") {
			return fmt.Errorf("invalid API key format for provider %s", c.ID)
		}
		return nil
	}

	switch c.Type {
	case catwalk.TypeOpenAI, catwalk.TypeOpenAICompat, catwalk.TypeOpenRouter:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.openai.com/v1")

		switch providerID {
		case catwalk.InferenceProviderOpenRouter:
			testURL = baseURL + "/credits"
		case catwalk.InferenceProviderOpenCodeGo:
			testURL = strings.Replace(baseURL, "/go", "", 1) + "/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["Authorization"] = "Bearer " + apiKey
	case catwalk.TypeAnthropic:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.anthropic.com/v1")

		switch providerID {
		case catwalk.InferenceKimiCoding:
			testURL = baseURL + "/v1/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	case catwalk.TypeGoogle:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://generativelanguage.googleapis.com")
		// The key goes in a header, never the URL: a failed request wraps
		// the full URL into the returned error (*url.Error), which ends up
		// in the UI and logs.
		testURL = baseURL + "/v1beta/models"
		headers["x-goog-api-key"] = apiKey
	case catwalk.TypeBedrock:
		// NOTE: Bedrock has a `/foundation-models` endpoint that we could in
		// theory use, but apparently the authorization is region-specific,
		// so it's not so trivial.
		if strings.HasPrefix(apiKey, "ABSK") { // Bedrock API keys
			return nil
		}
		return errors.New("not a valid bedrock api key")
	case catwalk.TypeVercel:
		// NOTE: Vercel does not validate API keys on the `/models` endpoint.
		if strings.HasPrefix(apiKey, "vck_") { // Vercel API keys
			return nil
		}
		return errors.New("not a valid vercel api key")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to provider %s: %w", c.ID, err)
	}
	defer resp.Body.Close()

	switch providerID {
	case catwalk.InferenceProviderZAI:
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	default:
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	}
	return nil
}
