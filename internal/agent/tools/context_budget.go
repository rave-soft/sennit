package tools

import (
	"context"
	"fmt"
	"sync"
)

type contextBudgetKey struct{}

const (
	// contextBudgetBytesPerToken converts the agent's free-token figure
	// into the byte budgets the read tools work in. Source code runs
	// denser than the four bytes per token the usage estimates assume, so
	// a cap uses three: it errs toward a shorter read.
	contextBudgetBytesPerToken = 3
	// minContextBudgetBytes is what a read is granted however full the
	// context is. A refusal would leave the model unable to look at
	// anything, and one line of MaxLineLength must always fit, or a read
	// returns no content and a cursor pointing at where it started.
	minContextBudgetBytes = 8 * 1024
)

// ContextBudget is how much tool output one step may still add before the
// session reaches the point where it stops to summarize. The agent creates
// one per step from what is free in the context window, and the read tools
// draw on it, so a file is never read into room that is not there.
//
// A step's tool calls are dispatched as the model streams them, some in
// parallel, so no call knows how many siblings it has. Each one is therefore
// granted at most half of what is left: the calls of a step cannot add up to
// more than the whole, and a single call still leaves room for the reply.
type ContextBudget struct {
	mu    sync.Mutex
	bytes int64
}

// NewContextBudget returns a budget for freeTokens of context. A value at or
// below zero is a context that is already past its limit; reads then get
// the minimum grant.
func NewContextBudget(freeTokens int64) *ContextBudget {
	return &ContextBudget{bytes: max(freeTokens, 0) * contextBudgetBytesPerToken}
}

// WithContextBudget returns ctx carrying b for the tools of one step.
func WithContextBudget(ctx context.Context, b *ContextBudget) context.Context {
	return context.WithValue(ctx, contextBudgetKey{}, b)
}

// reserveContextBudget grants up to want bytes of the step's budget and
// reports whether the grant is smaller than what was asked for. Without a
// budget on ctx (a model whose window is unknown, a tool run outside a
// turn) the request is granted in full. Pass what the call did not use to
// releaseContextBudget.
func reserveContextBudget(ctx context.Context, want int) (granted int, limited bool) {
	b, _ := ctx.Value(contextBudgetKey{}).(*ContextBudget)
	if b == nil {
		return want, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	grant := max(b.bytes/2, int64(minContextBudgetBytes))
	if grant >= int64(want) {
		grant = int64(want)
	}
	b.bytes = max(b.bytes-grant, 0)
	return int(grant), grant < int64(want)
}

// releaseContextBudget returns the unused part of a grant to the step's
// budget.
func releaseContextBudget(ctx context.Context, unused int) {
	b, _ := ctx.Value(contextBudgetKey{}).(*ContextBudget)
	if b == nil || unused <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bytes += int64(unused)
}

// fitContextBudget cuts text to what the step's context budget grants and
// says so at the cut. It is for tools whose whole result is one text with
// no page of its own to shorten; a tool that can page should reserve its
// size up front instead, so that it stops at a boundary it can resume from.
func fitContextBudget(ctx context.Context, text string) string {
	budget, limited := reserveContextBudget(ctx, len(text))
	if limited {
		text = truncateToRuneBoundary(text, budget) + fmt.Sprintf("\n\n[Content truncated to %d bytes: the context window is nearly full]", budget)
	}
	return text
}
