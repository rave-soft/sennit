package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type captureRoundTripper struct {
	body []byte
}

func (c *captureRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	c.body = append([]byte(nil), body...)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(
			`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)),
		Request: request,
	}, nil
}

type capturingLanguageModel struct {
	fantasy.LanguageModel
	captured fantasy.Call
}

func (m *capturingLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.captured = call
	return m.LanguageModel.Generate(ctx, call)
}

func TestMCPToolSchemaReachesOpenAIWireThroughAgent(t *testing.T) {
	source := map[string]any{
		"type":  "object",
		"$defs": map[string]any{"id": map[string]any{"type": "string", "pattern": "^[a-z]+$"}},
		"properties": map[string]any{
			"value": map[string]any{"$ref": "#/$defs/id"},
			"tags":  map[string]any{"type": "array", "prefixItems": []any{map[string]any{"type": "string"}}, "items": false},
		},
		"required":             []any{"value"},
		"additionalProperties": false,
	}
	expectedJSON, err := json.Marshal(source)
	require.NoError(t, err)
	var expected map[string]any
	require.NoError(t, json.Unmarshal(expectedJSON, &expected))
	mcpTool := &Tool{mcpName: "server", tool: &mcp.Tool{Name: "lookup", Description: "lookup a value", InputSchema: source}}
	transport := &captureRoundTripper{}
	provider, err := openai.New(
		openai.WithAPIKey("test-key"),
		openai.WithBaseURL("http://openai.test/v1"),
		openai.WithHTTPClient(&http.Client{Transport: transport}),
	)
	require.NoError(t, err)
	providerModel, err := provider.LanguageModel(t.Context(), "gpt-4o-mini")
	require.NoError(t, err)
	model := &capturingLanguageModel{LanguageModel: providerModel}

	agent := fantasy.NewAgent(model, fantasy.WithTools(mcpTool), fantasy.WithMaxRetries(0))
	_, err = agent.Generate(t.Context(), fantasy.AgentCall{Prompt: "use the lookup tool"})
	require.NoError(t, err)

	require.Len(t, model.captured.Tools, 1)
	prepared := model.captured.Tools[0].(fantasy.FunctionTool)
	require.Equal(t, expected, prepared.InputSchema)

	var wire struct {
		Tools []struct {
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(transport.body, &wire))
	require.Len(t, wire.Tools, 1)
	require.Equal(t, "mcp_server_lookup", wire.Tools[0].Function.Name)
	require.Equal(t, expected, wire.Tools[0].Function.Parameters)
	require.Equal(t, expected, source, "source schema was mutated")
}

func TestMCPToolInfoPreservesFullAndLegacySchemas(t *testing.T) {
	source := map[string]any{
		"type": "object", "$defs": map[string]any{"id": map[string]any{"type": "string"}},
		"properties": map[string]any{"value": map[string]any{"$ref": "#/$defs/id"}},
		"required":   []any{"value"}, "additionalProperties": false,
	}
	tool := &Tool{mcpName: "server", tool: &mcp.Tool{Name: "lookup", InputSchema: source}}
	info := tool.Info()
	require.Equal(t, source["properties"], info.Parameters)
	require.Equal(t, []string{"value"}, info.Required)
	require.Equal(t, source, info.InputSchema)
	info.InputSchema["additionalProperties"] = true
	require.False(t, source["additionalProperties"].(bool), "ToolInfo must not alias MCP SDK schema")
}
