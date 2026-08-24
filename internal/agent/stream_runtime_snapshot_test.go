package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// streamRuntimeCapturingModel records the concrete provider call. It is used
// with runWithStreamRuntime, rather than testing runtime fields directly, so
// this catches a second runtime assembly between the delegation budget and the
// actual provider request.
type streamRuntimeCapturingModel struct {
	provider string
	name     string

	mu    sync.Mutex
	calls []fantasy.Call
}

func (m *streamRuntimeCapturingModel) Provider() string { return m.provider }
func (m *streamRuntimeCapturingModel) Model() string    { return m.name }
func (m *streamRuntimeCapturingModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *streamRuntimeCapturingModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "done"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *streamRuntimeCapturingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *streamRuntimeCapturingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *streamRuntimeCapturingModel) turnCall(t *testing.T) fantasy.Call {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, call := range m.calls {
		if !isTitleCall(call) {
			return call
		}
	}
	t.Fatal("provider did not receive a turn call")
	return fantasy.Call{}
}

func TestRunWithStreamRuntimeUsesCapturedSnapshotEndToEnd(t *testing.T) {
	env := testEnv(t)
	capturedModel := &streamRuntimeCapturingModel{provider: "captured", name: "captured-model"}
	mutatedModel := &streamRuntimeCapturingModel{provider: "mutated", name: "mutated-model"}
	capturedTool := fantasy.NewAgentTool("captured_tool", "captured tool", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	mutatedTool := fantasy.NewAgentTool("mutated_tool", "mutated tool", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})

	sa := testSessionAgent(env, capturedModel, "captured system", capturedTool).(*sessionAgent)
	capturedRuntimeModel := sa.Model()
	capturedRuntimeModel.Model = capturedModel
	capturedRuntimeModel.ModelCfg = config.SelectedModel{Provider: "captured", Model: "captured-model"}
	capturedRuntimeModel.CatalogCfg.ContextWindow = 20_000
	capturedRuntimeModel.CatalogCfg.DefaultMaxTokens = 4_000
	sa.SetModel(capturedRuntimeModel)
	sa.systemPromptPrefix.Set("captured prefix")
	session, err := env.sessions.Create(t.Context(), "snapshot")
	require.NoError(t, err)

	// This is the same point at which runSubAgent captures its runtime after
	// readiness. Include MCP instructions in the captured prompt to verify the
	// request remains pinned even when the live prompt is changed afterward.
	runtime := sa.snapshotStreamRuntime(SessionAgentCall{SessionID: session.ID})
	runtime.systemPrompt += "\n\n<mcp-instructions>captured MCP instructions</mcp-instructions>"
	budgetBefore := carryOverBudget(carryOverBudgetInput{
		Model: runtime.model, SystemPromptBytes: len(runtime.systemPrompt) + len(runtime.systemPromptPrefix),
		ToolSchemaBytes: toolSchemaBytes(runtime.tools), PromptBytes: len("do work"),
	})

	// Simulate all mutable sources changing while a delegation waits to run.
	sa.SetModel(Model{Model: mutatedModel, ModelCfg: config.SelectedModel{Provider: "mutated", Model: "mutated-model"}})
	sa.SetTools([]fantasy.AgentTool{mutatedTool})
	sa.SetSystemPrompt("mutated system with mutated MCP instructions")
	sa.systemPromptPrefix.Set("mutated prefix")
	budgetAfterMutation := carryOverBudget(carryOverBudgetInput{
		Model: sa.Model(), SystemPromptBytes: len(sa.systemPrompt.Get()) + len(sa.systemPromptPrefix.Get()),
		ToolSchemaBytes: toolSchemaBytes(sa.tools.Copy()), PromptBytes: len("do work"),
	})
	require.NotEqual(t, budgetBefore, budgetAfterMutation, "fixture must make a fresh runtime budget observably different")

	_, err = sa.runWithStreamRuntime(t.Context(), SessionAgentCall{SessionID: session.ID, Prompt: "do work"}, runtime)
	require.NoError(t, err)
	require.Empty(t, mutatedModel.calls, "the mutated model must not receive the delegated request")

	call := capturedModel.turnCall(t)
	require.Equal(t, []string{"captured_tool"}, providerToolNames(call.Tools))
	require.True(t, promptHasRoleText(call.Prompt, fantasy.MessageRoleSystem, "captured system"))
	require.True(t, promptHasRoleText(call.Prompt, fantasy.MessageRoleSystem, "captured MCP instructions"))
	require.False(t, promptHasRoleText(call.Prompt, fantasy.MessageRoleSystem, "mutated system"))
	require.False(t, promptHasRoleText(call.Prompt, fantasy.MessageRoleSystem, "mutated MCP instructions"))
	require.Equal(t, 1, countSystemText(call.Prompt, "captured prefix"), "prefix must be a single separate system message")
	require.Zero(t, countSystemText(call.Prompt, "mutated prefix"))
}

type snapshotMutatingSessionAgent struct {
	*sessionAgent
	mutate func()
}

func (a *snapshotMutatingSessionAgent) snapshotStreamRuntime(call SessionAgentCall) streamRuntime {
	runtime := a.sessionAgent.snapshotStreamRuntime(call)
	a.mutate()
	return runtime
}

func TestRunSubAgentUsesOneRuntimeSnapshotForBudgetAndProvider(t *testing.T) {
	env := testEnv(t)
	coord := newTestCoordinator(t, env, config.ProviderConfig{ID: "captured"})
	coord.cfg.Config().Providers.Set("mutated", config.ProviderConfig{ID: "mutated"})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	const (
		agentID       = "snapshot-agent"
		delegation    = "do snapshot-sensitive work"
		capturedBase  = "captured system"
		capturedMCP   = "captured MCP instructions"
		capturedPref  = "captured prefix"
		mutatedSystem = "mutated system with mutated MCP instructions"
		mutatedPref   = "mutated prefix"
	)

	capturedProvider := &streamRuntimeCapturingModel{provider: "captured", name: "captured-model"}
	mutatedProvider := &streamRuntimeCapturingModel{provider: "mutated", name: "mutated-model"}
	capturedTool := fantasy.NewAgentTool("captured_tool", "captured tool", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	mutatedTool := fantasy.NewAgentTool("mutated_tool", "mutated tool", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})

	sa := testSessionAgent(env, capturedProvider, capturedBase, capturedTool).(*sessionAgent)
	capturedModel := sa.Model()
	capturedModel.Model = capturedProvider
	capturedModel.ModelCfg = config.SelectedModel{Provider: "captured", Model: "captured-model"}
	capturedModel.CatalogCfg.ContextWindow = 14_000
	capturedModel.CatalogCfg.DefaultMaxTokens = 1_000
	sa.SetModel(capturedModel)
	sa.SetSystemPrompt(capturedBase + "\n\n<mcp-instructions>" + capturedMCP + "</mcp-instructions>")
	sa.systemPromptPrefix.Set(capturedPref)

	capturedRuntime := sa.snapshotStreamRuntime(SessionAgentCall{})
	capturedBudget := carryOverBudget(carryOverBudgetInput{
		Model: capturedRuntime.model, SystemPromptBytes: len(capturedRuntime.systemPrompt) + len(capturedRuntime.systemPromptPrefix),
		ToolSchemaBytes: toolSchemaBytes(capturedRuntime.tools), PromptBytes: len(delegation),
	})
	mutatedModel := capturedModel
	mutatedModel.Model = mutatedProvider
	mutatedModel.ModelCfg = config.SelectedModel{Provider: "mutated", Model: "mutated-model"}
	mutatedModel.CatalogCfg.ContextWindow = 30_000
	mutatedModel.CatalogCfg.DefaultMaxTokens = 1_000
	mutatedBudget := carryOverBudget(carryOverBudgetInput{
		Model: mutatedModel, SystemPromptBytes: len(mutatedSystem) + len(mutatedPref),
		ToolSchemaBytes: toolSchemaBytes([]fantasy.AgentTool{mutatedTool}), PromptBytes: len(delegation),
	})
	require.Greater(t, mutatedBudget, capturedBudget)

	oldSession, err := env.sessions.CreateSubAgentSession(t.Context(), "old-call", parent.ID, "Old delegation", agentID)
	require.NoError(t, err)
	oldText := strings.Repeat("old-history-", capturedBudget/12+2_000)
	_, err = env.messages.Create(t.Context(), oldSession.ID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: oldText}},
	})
	require.NoError(t, err)

	wantPrior := trimToBudget([]message.Message{{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: oldText}},
	}}, capturedBudget)
	livePrior := trimToBudget([]message.Message{{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: oldText}},
	}}, mutatedBudget)
	require.NotEqual(t, messagesTextLen(wantPrior), messagesTextLen(livePrior), "fixture must distinguish captured and live budgets")

	// The controlled subtype mutates the live agent immediately after returning
	// the production snapshot, before carry-over budgeting and dispatch.
	agent := &snapshotMutatingSessionAgent{sessionAgent: sa, mutate: func() {
		sa.SetModel(mutatedModel)
		sa.SetTools([]fantasy.AgentTool{mutatedTool})
		sa.SetSystemPrompt(mutatedSystem)
		sa.systemPromptPrefix.Set(mutatedPref)
	}}
	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent: agent, SessionID: parent.ID, AgentMessageID: "new-message", ToolCallID: "new-call",
		Prompt: delegation, SessionTitle: "New delegation", AgentID: agentID,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.Empty(t, mutatedProvider.calls, "the live model must not receive the request")

	call := capturedProvider.turnCall(t)
	require.Equal(t, []string{"captured_tool"}, providerToolNames(call.Tools))
	require.True(t, promptHasRoleText(call.Prompt, fantasy.MessageRoleSystem, capturedBase))
	require.True(t, promptHasRoleText(call.Prompt, fantasy.MessageRoleSystem, capturedMCP))
	require.False(t, promptHasRoleText(call.Prompt, fantasy.MessageRoleSystem, mutatedSystem))
	require.Equal(t, 1, countSystemText(call.Prompt, capturedPref))
	require.Zero(t, countSystemText(call.Prompt, mutatedPref))
	carriedLen := longestPromptRoleText(call.Prompt, fantasy.MessageRoleAssistant)
	require.Equal(t, messagesTextLen(wantPrior), carriedLen, "provider must receive history trimmed to the captured runtime budget")
	require.NotEqual(t, messagesTextLen(livePrior), carriedLen, "provider history must not use the mutated live runtime budget")
}

func longestPromptRoleText(prompt fantasy.Prompt, role fantasy.MessageRole) int {
	longest := 0
	for _, msg := range prompt {
		if msg.Role != role {
			continue
		}
		for _, part := range msg.Content {
			if part, ok := part.(fantasy.TextPart); ok {
				longest = max(longest, len(part.Text))
			}
		}
	}
	return longest
}

func providerToolNames(tools []fantasy.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.GetName())
	}
	slices.Sort(names)
	return names
}

// countSystemText counts the system messages carrying exactly text. Only the
// system role is ever asked about: the prompt prefix is what these tests pin.
func countSystemText(prompt fantasy.Prompt, text string) int {
	count := 0
	for _, msg := range prompt {
		if msg.Role != fantasy.MessageRoleSystem {
			continue
		}
		for _, part := range msg.Content {
			if part, ok := part.(fantasy.TextPart); ok && part.Text == text {
				count++
			}
		}
	}
	return count
}
