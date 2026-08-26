package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

// modelsTimeout bounds the model-list call. It runs on the sign-in path and
// on config load, so it must fail fast rather than hold either up.
const modelsTimeout = 10 * time.Second

// apiBaseURL is APIBaseURL behind a variable so tests can point requests at
// an httptest server, the same trick codex.go plays with callbackPort.
// Production code never assigns to this.
var apiBaseURL = APIBaseURL

// modelEntry is the subset of the Codex model list Sennit uses. The endpoint
// returns a great deal more (prompt templates, tool modes, NUX copy) that
// belongs to the Codex CLI's own UI, not to a model catalog.
type modelEntry struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	// ContextWindow is the baseline window, not the ceiling: the backend
	// publishes it alongside MaxContextWindow, which is what it actually
	// enforces. For gpt-5.6 the two are 272k and 872k, and a request of
	// 877k input tokens is accepted while ~1.08M is rejected with
	// context_length_exceeded; for gpt-5.5, where both read 272k, 400k is
	// rejected. See toCatwalk.
	ContextWindow    int64    `json:"context_window"`
	MaxContextWindow int64    `json:"max_context_window"`
	Visibility       string   `json:"visibility"`
	SupportedInAPI   bool     `json:"supported_in_api"`
	InputModals      []string `json:"input_modalities"`
	DefaultLevel     string   `json:"default_reasoning_level"`
	ReasoningLevel   []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
}

// newModelsRequest builds the GET /models request that both FetchModels and
// FetchUsage send — same URL, same headers, same proxy-aware client. The two
// callers only differ in which part of the response they read: one the
// body, the other the X-Codex-* headers riding along with it.
func newModelsRequest(ctx context.Context, proxyURL, accessToken, accountID string) (*http.Request, *http.Client, error) {
	// client_version is mandatory here: the endpoint answers 400 without it,
	// since which models it lists depends on what the client can drive.
	url := apiBaseURL + "/models?client_version=" + clientVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	for k, v := range Headers(accountID) {
		req.Header.Set(k, v)
	}

	client, err := httpClient(proxyURL, modelsTimeout)
	if err != nil {
		return nil, nil, err
	}
	return req, client, nil
}

// FetchModels asks the Codex backend which models the signed-in account can
// use. The list is per-account (plans differ, and models come and go), which
// is why it is fetched rather than shipped in the catalog.
func FetchModels(ctx context.Context, proxyURL, accessToken, accountID string) ([]catwalk.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	req, client, err := newModelsRequest(ctx, proxyURL, accessToken, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex model list failed: %s: %s", resp.Status, truncate(string(body), 200))
	}
	return parseModels(body)
}

// parseModels converts a model-list response body into catalog models.
func parseModels(body []byte) ([]catwalk.Model, error) {
	// The endpoint has returned both a bare array and an object wrapping
	// one, so accept either rather than breaking on the shape of the day.
	var entries []modelEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		var wrapper struct {
			Models []modelEntry `json:"models"`
			Data   []modelEntry `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil {
			return nil, fmt.Errorf("could not parse the codex model list: %w", err)
		}
		entries = wrapper.Models
		if len(entries) == 0 {
			entries = wrapper.Data
		}
	}

	models := make([]catwalk.Model, 0, len(entries))
	for _, entry := range entries {
		if model, ok := entry.toCatwalk(); ok {
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("the codex model list came back empty")
	}
	return models, nil
}

// toCatwalk converts one entry, reporting false for models the account is
// not meant to pick from: hidden entries and ones the endpoint marks as not
// callable over the API.
func (e modelEntry) toCatwalk() (catwalk.Model, bool) {
	if e.Slug == "" || !e.SupportedInAPI {
		return catwalk.Model{}, false
	}
	// Anything the backend does not mark for listing ("hide", and whatever
	// else it grows) is meant for Codex's own internal use, not for a model
	// picker.
	if e.Visibility != "" && e.Visibility != "list" {
		return catwalk.Model{}, false
	}

	levels := make([]string, 0, len(e.ReasoningLevel))
	for _, level := range e.ReasoningLevel {
		if level.Effort != "" {
			levels = append(levels, level.Effort)
		}
	}

	name := e.DisplayName
	if name == "" {
		name = e.Slug
	}
	// The larger of the two published windows is the one the backend
	// enforces; taking the smaller made Sennit compact and warn at a third
	// of the room the account actually has.
	contextWindow := max(e.ContextWindow, e.MaxContextWindow)
	if contextWindow <= 0 {
		contextWindow = 272_000
	}

	return catwalk.Model{
		ID:            e.Slug,
		Name:          name,
		ContextWindow: contextWindow,
		// No output cap: the endpoint publishes none and rejects the
		// parameter outright ("Unsupported parameter:
		// max_output_tokens"), so zero — "unknown" — is both honest and
		// what keeps the field off the wire.
		DefaultMaxTokens: 0,
		// A ChatGPT subscription is a flat fee: per-token pricing would
		// report a cost the user is not being charged.
		CostPer1MIn:            0,
		CostPer1MOut:           0,
		CanReason:              len(levels) > 0,
		ReasoningLevels:        levels,
		DefaultReasoningEffort: e.DefaultLevel,
		SupportsImages:         slices.Contains(e.InputModals, "image"),
	}, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
