package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/providers/typeclass"
	"github.com/rave-soft/sennit/internal/proxyhttp"
)

type Provider = state.Provider

func FromConfig(c config.ProviderConfig, resolver config.VariableResolver) (Provider, error) {
	apiKey, err := resolver.ResolveValue(c.APIKey)
	if err != nil {
		return Provider{}, fmt.Errorf("resolving API key for provider %s: %w", c.ID, err)
	}
	baseURL, err := resolver.ResolveValue(c.BaseURL)
	if err != nil {
		return Provider{}, fmt.Errorf("resolving base URL for provider %s: %w", c.ID, err)
	}
	headers := maps.Clone(c.ExtraHeaders)
	if err := config.ResolveProviderHeaders(headers, resolver, c.ID); err != nil {
		return Provider{}, err
	}
	proxyURL := config.ResolveOptionalProviderProxy(c.ProxyURL, resolver, c.ID)
	p := Provider{ID: c.ID, Name: c.Name, BaseURL: baseURL, Type: c.Type, APIKey: apiKey, APIKeyTemplate: c.APIKey, OAuthToken: c.OAuthToken, ProxyURL: proxyURL, ConfiguredProxyURL: proxyURL, Account: c.Account, ExtraHeaders: headers, ExtraParams: make(map[string]string), Models: c.Models}
	switch catwalk.InferenceProvider(c.ID) {
	case catwalk.InferenceProviderAzure:
		p.ExtraParams["apiVersion"], _ = resolver.ResolveValue("$AZURE_OPENAI_API_VERSION")
	case catwalk.InferenceProviderVertexAI:
		p.ExtraParams["project"], _ = resolver.ResolveValue("$VERTEXAI_PROJECT")
		p.ExtraParams["location"], _ = resolver.ResolveValue("$VERTEXAI_LOCATION")
	}
	ApplyPostCredentialSetup(&p)
	return p, nil
}

func ApplyPostCredentialSetup(p *Provider) {
	if p.ExtraHeaders == nil {
		p.ExtraHeaders = make(map[string]string)
	}
	switch p.ID {
	case string(catwalk.InferenceProviderCopilot):
		maps.Copy(p.ExtraHeaders, copilot.Headers())
	case codex.ProviderID:
		accountID := codex.AccountID(p.APIKey)
		if accountID == "" && p.OAuthToken != nil {
			accountID = codex.AccountID(p.OAuthToken.AccessToken)
		}
		if accountID == "" {
			delete(p.ExtraHeaders, codex.AccountIDHeader)
		}
		maps.Copy(p.ExtraHeaders, codex.Headers(accountID))
	}
}

func ToProvider(c Provider) catwalk.Provider {
	return catwalk.Provider{Name: c.Name, ID: catwalk.InferenceProvider(c.ID), Models: c.Models}
}

func Providers(cfg *config.Config) []catwalk.Provider {
	if cfg.Options.DisableDefaultProviders {
		return nil
	}
	return append(embedded.GetAll(), CodexProvider())
}

func CodexProvider() catwalk.Provider {
	return catwalk.Provider{ID: catwalk.InferenceProvider(codex.ProviderID), Name: codex.ProviderName, APIEndpoint: codex.APIBaseURL, Type: catwalk.TypeOpenAI}
}

func TestConnection(c Provider, resolver config.VariableResolver) error {
	providerID := catwalk.InferenceProvider(c.ID)
	testURL := ""
	headers := make(map[string]string)
	apiKey := c.APIKey
	if apiKey == "" {
		var err error
		apiKey, err = resolver.ResolveValue(c.APIKeyTemplate)
		if err != nil {
			return fmt.Errorf("failed to resolve API key for provider %s: %w", c.ID, err)
		}
	}
	switch providerID {
	case catwalk.InferenceProviderMiniMax, catwalk.InferenceProviderMiniMaxChina:
		return nil
	case catwalk.InferenceProviderAlibabaSingapore:
		if !strings.HasPrefix(apiKey, "sk-") {
			return fmt.Errorf("invalid API key format for provider %s", c.ID)
		}
		return nil
	}
	baseURL := c.BaseURL
	var err error
	if baseURL != "" {
		baseURL, err = resolver.ResolveValue(baseURL)
		if err != nil {
			return fmt.Errorf("failed to resolve base URL for provider %s: %w", c.ID, err)
		}
	}
	switch typeclass.Of(c.Type) {
	case typeclass.OpenAI, typeclass.OpenAICompat, typeclass.OpenRouter:
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
	case typeclass.Anthropic:
		baseURL = cmp.Or(baseURL, "https://api.anthropic.com/v1")
		if providerID == catwalk.InferenceKimiCoding {
			testURL = baseURL + "/v1/models"
		} else {
			testURL = baseURL + "/models"
		}
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	case typeclass.Google:
		baseURL = cmp.Or(baseURL, "https://generativelanguage.googleapis.com")
		testURL = baseURL + "/v1beta/models"
		headers["x-goog-api-key"] = apiKey
	case typeclass.Bedrock:
		if strings.HasPrefix(apiKey, "ABSK") {
			return nil
		}
		return errors.New("not a valid bedrock api key")
	case typeclass.Vercel:
		if strings.HasPrefix(apiKey, "vck_") {
			return nil
		}
		return errors.New("not a valid vercel api key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	if c.ProxyURL != "" {
		client, err = proxyhttp.NewClient(c.ProxyURL, 5*time.Second)
		if err != nil {
			return fmt.Errorf("failed to build proxy client for provider %s: %w", c.ID, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
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
	if providerID == catwalk.InferenceProviderZAI {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	} else if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
	}
	return nil
}
