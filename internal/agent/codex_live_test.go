package agent

import (
	"context"
	"os"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

// TestLiveCodexStream drives the real Codex backend through the same fantasy
// provider Sennit builds for it, which is where a wire-format mismatch would
// surface. It covers a streamed reply and a tool call, since tool calling is
// what a coding agent actually does with the provider.
//
// Skipped unless CODEX_LIVE=1; it needs a signed-in ChatGPT account.
//
//	CODEX_LIVE=1 go test ./internal/agent/ -run TestLiveCodex -v
func TestLiveCodexStream(t *testing.T) {
	if os.Getenv("CODEX_LIVE") != "1" {
		t.Skip("set CODEX_LIVE=1 to run against the real Codex backend")
	}
	ctx := context.Background()
	proxy := os.Getenv("CODEX_PROXY")

	disk, ok := codex.TokensFromDisk()
	require.True(t, ok, "no Codex CLI login on disk to test with")
	token, err := codex.RefreshToken(ctx, proxy, disk.RefreshToken)
	require.NoError(t, err)
	accountID := codex.AccountID(token.AccessToken)

	models, err := codex.FetchModels(ctx, proxy, token.AccessToken, accountID)
	require.NoError(t, err)
	require.NotEmpty(t, models)

	// Built exactly as coordinator.buildOpenaiProvider does for a provider
	// of type openai, so this tests the shipped configuration.
	provider, err := openai.New(
		openai.WithAPIKey(token.AccessToken),
		openai.WithUseResponsesAPI(),
		openai.WithBaseURL(codex.APIBaseURL),
		openai.WithHeaders(codex.Headers(accountID)),
	)
	require.NoError(t, err)

	model, err := provider.LanguageModel(ctx, models[0].ID)
	require.NoError(t, err)

	opts, err := openai.ParseResponsesOptions(map[string]any{
		"reasoning_effort":  "low",
		"reasoning_summary": "auto",
		"include":           []openai.IncludeType{openai.IncludeReasoningEncryptedContent},
	})
	require.NoError(t, err)

	// Streaming only: the Codex endpoint answers a non-streaming request
	// with "Stream must be set to true". That costs Sennit nothing, since
	// every model call it makes goes through Stream.
	stream, err := model.Stream(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{
			fantasy.NewUserMessage("Read the file /etc/hostname using the read_file tool."),
		},
		Tools: []fantasy.Tool{fantasy.FunctionTool{
			Name:        "read_file",
			Description: "Read a file from disk.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			},
		}},
		ProviderOptions: fantasy.ProviderOptions{openai.Name: opts},
	})
	require.NoError(t, err)

	var (
		toolCall     *fantasy.StreamPart
		finishReason fantasy.FinishReason
		usage        fantasy.Usage
	)
	for part := range stream {
		require.NoError(t, part.Error)
		switch part.Type {
		case fantasy.StreamPartTypeToolCall:
			toolCall = &part
		case fantasy.StreamPartTypeFinish:
			finishReason = part.FinishReason
			usage = part.Usage
		}
	}

	require.NotNil(t, toolCall, "the model must be able to call a tool through this endpoint")
	require.Equal(t, "read_file", toolCall.ToolCallName)
	require.Contains(t, toolCall.ToolCallInput, "/etc/hostname")
	require.Equal(t, fantasy.FinishReasonToolCalls, finishReason)
	require.Positive(t, usage.InputTokens, "usage must come back so the session can account for it")
}
