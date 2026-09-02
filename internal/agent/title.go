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
	"golang.org/x/sync/errgroup"

	"github.com/rave-soft/sennit/internal/message"
)

const DefaultSessionName = "Untitled Session"

// titleGenerationTimeout bounds one title-generation call. A session title
// is a single line derived from the opening prompt — not worth holding a
// stalled provider's HTTP request (and this goroutine) open indefinitely
// for; a generous but finite window keeps a slow-but-healthy provider
// working while still bounding the leak a stalled one would otherwise
// cause. See internal/agent/provider_stall.go for the watchdog that
// protects calls that matter more than this one. Overridable per agent
// (sessionAgent.titleTimeout) so a test can prove the bound is applied
// without paying the production value in wall-clock time.
const titleGenerationTimeout = 45 * time.Second

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

// startGenerateTitle launches generateTitle asynchronously. The call
// context is detached (context.WithoutCancel) so the goroutine survives
// the turn that triggered it, then bounded by titleGenerationTimeout so it
// does not survive forever — a stalled provider must not leak the
// goroutine or its in-flight request past the turn.
//
// When a carries a readiness lifecycle (the coordinator's own agent, and
// agents built through delegationFinalizer), the work is launched through
// lifecycle.launch so Coordinator.Close observes and waits for it instead
// of being unaware of a bare goroutine. A sessionAgent built without one
// (most tests, and one-off agents that never go through buildAgent) falls
// back to a plain goroutine — nothing to register with in that case.
func (a *sessionAgent) startGenerateTitle(ctx context.Context, sessionID, userPrompt string, model Model, systemPromptPrefix string) {
	timeout := a.titleTimeout
	if timeout <= 0 {
		timeout = titleGenerationTimeout
	}
	titleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	work := func(context.Context) error {
		defer cancel()
		a.generateTitle(titleCtx, sessionID, userPrompt, model, systemPromptPrefix)
		return nil
	}
	// A fresh group per call: nothing needs to wait on this specific
	// title generation, only Close needs to know it exists — and
	// readinessLifecycle.launch tracks that on its own internal
	// WaitGroup regardless of which *errgroup.Group is passed in (see
	// delegationFinalizer.buildAgent's identical use for a sub-agent's
	// readiness group).
	if a.lifecycle == nil || !a.lifecycle.launch(&errgroup.Group{}, work) {
		go func() { _ = work(context.Background()) }()
	}
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

	// Calculate usage and cost, through the same helpers the per-turn
	// path uses: this used to carry its own copy of both the cost formula
	// and the token accounting, and the token halves had drifted — the
	// turn path counts cache *reads* as prompt tokens and this counted
	// cache *creations*, so a session's totals depended on which of the
	// two wrote last.
	cost := catalogCost(model, resp.TotalUsage)
	if openrouterCost := a.openrouterTotal(resp.Steps); openrouterCost != nil {
		cost = *openrouterCost
	}
	if model.FlatRate {
		cost = 0
	}

	promptTokens := promptTokensOf(resp.TotalUsage)
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
