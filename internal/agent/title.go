package agent

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
)

const DefaultSessionName = "Untitled Session"

//go:embed templates/title.md
var titlePrompt []byte

// Used to remove <think> tags from generated titles.
var (
	thinkTagRegex       = regexp.MustCompile(`(?s)<think>.*?</think>`)
	orphanThinkTagRegex = regexp.MustCompile(`</?think>`)
)

// hasUserTextMessage reports whether msgs already contains a
// message.User message with text — either something the user typed
// themselves or a delegation's goal/follow-up dispatched with
// message.OriginAgent (Role is always message.User regardless of
// origin; see SessionAgentCall.PromptOrigin). Either way generateTitle
// already ran once for this session and must not run again and clobber
// that title with a shorter follow-up prompt.
func hasUserTextMessage(msgs []message.Message) bool {
	for _, msg := range msgs {
		if msg.Role != message.User {
			continue
		}
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok && tc.Text != "" {
				return true
			}
		}
	}
	return false
}

// GenerateTitle generates a session title based on the initial prompt.
func (a *sessionAgent) GenerateTitle(ctx context.Context, sessionID string, userPrompt string) {
	a.generateTitle(ctx, sessionID, userPrompt, a.model.Get(), a.systemPromptPrefix.Get())
}

// generateTitle titles the session with a single call on model. There is no
// fallback between models: if the call fails, the deferred fallback below
// saves the default session name.
func (a *sessionAgent) generateTitle(ctx context.Context, sessionID string, userPrompt string, model Model, systemPromptPrefix string) {
	if userPrompt == "" {
		return
	}

	// Ensure the session always gets a title even if every path below
	// fails or the context is cancelled before we finish.
	var titleSaved bool
	defer func() {
		if !titleSaved {
			fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := a.sessions.Rename(fallbackCtx, sessionID, DefaultSessionName); err != nil {
				slog.Error("Failed to save fallback session title", "error", err)
			}
		}
	}()

	newAgent := func(m fantasy.LanguageModel, p []byte, tok int64) fantasy.Agent {
		opts := []fantasy.AgentOption{
			fantasy.WithSystemPrompt(string(p) + "\n /no_think"),
			fantasy.WithUserAgent(userAgent),
		}
		// A zero cap means the provider will not take one (Codex rejects
		// the field outright), so the option is left off rather than
		// sending a limit of nothing.
		if tok > 0 {
			opts = append(opts, fantasy.WithMaxOutputTokens(tok))
		}
		return fantasy.NewAgent(m, opts...)
	}

	streamCall := fantasy.AgentStreamCall{
		Prompt:  fmt.Sprintf("Generate a concise title for the following content:\n\n%s\n <think>\n\n</think>", userPrompt),
		Headers: sessionHeaders(sessionID),
		PrepareStep: func(callCtx context.Context, opts fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = opts.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{
					fantasy.NewSystemMessage(systemPromptPrefix),
				}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
	}

	// A title is one line, so a tiny cap is plenty; a reasoning model needs
	// room to think first and gets its own. A provider that takes no cap at
	// all gets none — the small default would be rejected like any other
	// value.
	tok := int64(40)
	if model.CatalogCfg.CanReason {
		tok = modelMaxOutputTokens(model)
	}
	if rejectsMaxOutputTokens(model) {
		tok = 0
	}
	agent := newAgent(model.Model, titlePrompt, tok)
	resp, err := agent.Stream(ctx, streamCall)
	if err != nil {
		// The deferred fallback will save the default session name.
		slog.Error("Failed to generate title", "err", err)
		return
	}
	if resp.Response.FinishReason == fantasy.FinishReasonLength {
		slog.Error("Title generation hit the token limit")
		return
	}

	// Clean up title.
	var title string
	title = strings.ReplaceAll(resp.Response.Content.Text(), "\n", " ")

	// Remove thinking tags if present.
	title = thinkTagRegex.ReplaceAllString(title, "")
	title = orphanThinkTagRegex.ReplaceAllString(title, "")

	title = strings.TrimSpace(title)
	if title == "" {
		// LLM returned empty content. Use the prompt itself as a
		// fallback title, truncated to 50 chars, before resorting to
		// the generic default.
		fallback := strings.ReplaceAll(userPrompt, "\n", " ")
		fallback = strings.TrimSpace(fallback)
		if len(fallback) > 50 {
			fallback = truncateRunes(fallback, 50)
		}
		title = cmp.Or(fallback, DefaultSessionName)
	}

	// Calculate usage and cost.
	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	modelConfig := model.CatalogCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(resp.TotalUsage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(resp.TotalUsage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(resp.TotalUsage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(resp.TotalUsage.OutputTokens)

	// Use override cost if available (e.g., from OpenRouter).
	if openrouterCost != nil {
		cost = *openrouterCost
	}

	// Skip cost accumulation
	if model.FlatRate {
		cost = 0
	}

	promptTokens := resp.TotalUsage.InputTokens + resp.TotalUsage.CacheCreationTokens
	completionTokens := resp.TotalUsage.OutputTokens

	// Atomically update only title and usage fields to avoid overriding other
	// concurrent session updates.
	saveErr := a.sessions.UpdateTitleAndUsage(ctx, sessionID, title, promptTokens, completionTokens, cost)
	if saveErr != nil {
		slog.Error("Failed to save session title and usage", "error", saveErr)
		return
	}
	titleSaved = true
}

// truncateRunes truncates s to at most n runes, appending "…" when
// truncation occurred. It's a dependency-free stand-in for ANSI-width-aware
// truncation, good enough for a plain-text session-title fallback.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
