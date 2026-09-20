package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// TestLiveCodexPromptCache measures what the Codex backend actually caches
// for the requests Sennit sends it. The logs show roughly half of a
// session's requests coming back with no cached tokens at all while a
// local llama.cpp server caches nearly everything from the same
// conversation, and no amount of reading the request builder settles
// whether that is the prompt drifting or the backend behaving that way.
//
// Skipped unless CODEX_LIVE=1; it spends a few thousand tokens of the
// signed-in account's allowance.
//
//	CODEX_LIVE=1 go test ./internal/agent/ -run TestLiveCodexPromptCache -v
func TestLiveCodexPromptCache(t *testing.T) {
	if os.Getenv("CODEX_LIVE") != "1" {
		t.Skip("set CODEX_LIVE=1 to run against the real Codex backend")
	}
	ctx := context.Background()
	proxy := cmp.Or(os.Getenv("CODEX_PROXY"), configuredCodexProxy(t))

	// The account Sennit itself is signed in as, rather than whichever one
	// the Codex CLI holds: with several accounts on file they are rarely
	// the same, and a probe run against a spent one measures nothing.
	// CODEX_ACCOUNT names a different account id to use.
	accessToken := storedCodexAccessToken(t, os.Getenv("CODEX_ACCOUNT"))
	if accessToken == "" {
		disk, ok := codex.TokensFromDisk()
		require.True(t, ok, "no Codex login to test with")
		token, ok := disk.Token()
		require.True(t, ok, "the Codex CLI login on disk has no usable access token")
		accessToken = token.AccessToken
	}
	accountID := codex.AccountID(accessToken)

	models, err := codex.FetchModels(ctx, proxy, accessToken, accountID)
	require.NoError(t, err)
	require.NotEmpty(t, models)

	// Built exactly as buildOpenaiProvider does for Codex, proxy included:
	// the account this runs against may only be reachable through one.
	httpClient, err := buildProviderHTTPClient(proxy, false)
	require.NoError(t, err)
	providerOpts := []openai.Option{
		openai.WithAPIKey(accessToken),
		openai.WithUseResponsesAPI(),
		openai.WithResponsesAPIFunc(func(string) bool { return true }),
		openai.WithBaseURL(codex.APIBaseURL),
		openai.WithHeaders(codex.Headers(accountID)),
	}
	if httpClient != nil {
		providerOpts = append(providerOpts, openai.WithHTTPClient(httpClient))
	}
	provider, err := openai.New(providerOpts...)
	require.NoError(t, err)
	model, err := provider.LanguageModel(ctx, models[0].ID)
	require.NoError(t, err)

	// A prefix well past the 1024-token minimum OpenAI caches from, and
	// stable to the byte between requests.
	// Size is a variable of the experiment: the smallest cache hit seen in
	// a real session's logs was around 13k tokens, so a few thousand may
	// simply be under whatever this backend caches from.
	repeats := 400
	if n, convErr := strconv.Atoi(os.Getenv("CODEX_CACHE_REPEATS")); convErr == nil && n > 0 {
		repeats = n
	}
	prefix := strings.Repeat("The quick brown fox jumps over the lazy dog. ", repeats)

	prompt := fantasy.Prompt{
		fantasy.NewUserMessage("<system_reminder>no todos</system_reminder>"),
		fantasy.NewUserMessage(prefix),
		fantasy.NewUserMessage("Answer with the single word: ok."),
	}

	opts := func(cacheKey string) fantasy.ProviderOptions {
		o, parseErr := openai.ParseResponsesOptions(map[string]any{
			"reasoning_effort":  "low",
			"reasoning_summary": "auto",
			"include":           []openai.IncludeType{openai.IncludeReasoningEncryptedContent},
		})
		require.NoError(t, parseErr)
		if cacheKey != "" {
			o.PromptCacheKey = &cacheKey
		}
		return fantasy.ProviderOptions{openai.Name: o}
	}

	ask := func(t *testing.T, headers map[string]string, providerOptions fantasy.ProviderOptions) fantasy.Usage {
		t.Helper()
		stream, streamErr := model.Stream(ctx, fantasy.Call{
			Prompt:          prompt,
			Headers:         headers,
			ProviderOptions: providerOptions,
		})
		require.NoError(t, streamErr)
		var usage fantasy.Usage
		for part := range stream {
			require.NoError(t, part.Error)
			if part.Type == fantasy.StreamPartTypeFinish {
				usage = part.Usage
			}
		}
		return usage
	}

	// Each variant sends the identical prompt twice: the first request can
	// only ever populate the cache, so it is the second one that says
	// whether this shape of request is cacheable at all.
	rounds := 2
	if n, convErr := strconv.Atoi(os.Getenv("CODEX_CACHE_ROUNDS")); convErr == nil && n > 0 {
		rounds = n
	}
	probe := func(name string, headers map[string]string, providerOptions fantasy.ProviderOptions) {
		for i := 1; i <= rounds; i++ {
			u := ask(t, headers, providerOptions)
			t.Logf("%-28s round %d: input=%-6d cached=%-6d", name, i, u.InputTokens, u.CacheReadTokens)
		}
	}

	sennitKey := session.HashID("codex-cache-live-test")
	sennitHeaders := sessionHeaders("codex-cache-live-test", codex.ProviderID)
	uuidSession := "9f1c2b7e-3a45-4c8d-9e10-5f6a7b8c9d01"

	switch os.Getenv("CODEX_CACHE_VARIANT") {
	case "uuid":
		// What the Codex CLI sends: the conversation's UUID, and its own
		// session_id header alongside it.
		probe("uuid cache key", map[string]string{"session_id": uuidSession}, opts(uuidSession))
	case "headers":
		probe("+ session_id header", map[string]string{
			"x-session-id":       sennitKey,
			"x-session-affinity": sennitKey,
			"session_id":         uuidSession,
		}, opts(sennitKey))
	case "none":
		probe("no key, no headers", nil, opts(""))
	default:
		probe("as sennit sends it", sennitHeaders, opts(sennitKey))
	}
}

// configuredCodexProxy reads the proxy the signed-in Codex provider is
// configured with, so a live run needs no secret on its command line. An
// account reachable without one simply has none, which is not an error.
func configuredCodexProxy(t *testing.T) string {
	t.Helper()
	// The package's TestMain points every config path at a throwaway
	// directory (see common_test.go), which is what keeps ordinary tests
	// away from the developer's own state. A live run is the one case that
	// wants that state, so the real path is resolved here directly.
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dataHome := cmp.Or(os.Getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share"))
	data, err := os.ReadFile(filepath.Join(dataHome, "sennit", "sennit.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Providers map[string]struct {
			ProxyURL string `json:"proxy_url"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.Providers[codex.ProviderID].ProxyURL
}

// storedCodexAccessToken returns the access token Sennit holds for
// accountID, or for its active Codex account when accountID is empty. It
// reads the account store directly for the same reason
// configuredCodexProxy reads the config directly: this package's TestMain
// points every path at a throwaway directory.
func storedCodexAccessToken(t *testing.T, accountID string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	configHome := cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
	data, err := os.ReadFile(filepath.Join(configHome, "sennit", "accounts.json"))
	if err != nil {
		return ""
	}
	var store struct {
		Accounts map[string][]struct {
			ID    string `json:"id"`
			Token struct {
				AccessToken string `json:"access_token"`
			} `json:"token"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return ""
	}
	if accountID == "" {
		accountID = activeCodexAccountID(t)
	}
	for _, a := range store.Accounts[codex.ProviderID] {
		if a.ID == accountID || accountID == "" {
			return a.Token.AccessToken
		}
	}
	return ""
}

// activeCodexAccountID reads which stored account Codex requests currently
// go out as.
func activeCodexAccountID(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dataHome := cmp.Or(os.Getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share"))
	data, err := os.ReadFile(filepath.Join(dataHome, "sennit", "sennit.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Providers map[string]struct {
			Account string `json:"account"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.Providers[codex.ProviderID].Account
}
