package lsp

import (
	"context"
	"sync/atomic"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
)

// clientGeneration is one running LSP process together with its context and
// the cancellation that owns it. A Client never mutates a generation in
// place; it swaps whole generations on restart, and diagnostic events carry
// the generation they were produced for so a stale generation's traffic can
// be dropped after the swap.
type clientGeneration struct {
	client *powernap.Client
	ctx    context.Context
	cancel context.CancelFunc
	// retired is the publication gate: it is set under the shared
	// publication lock (dispatchGeneration) the instant a generation stops
	// being current — either because a new generation has been published
	// (restart) or because the client is shutting down. It makes
	// generation retirement atomic with every other generation-dependent
	// publication, so no reader can observe the old generation as current
	// while the diagnostics store already moved on (or vice versa).
	retired atomic.Bool
	// dead is true once the process behind the generation is definitively
	// closed or killed. The lifecycle cancels the context of a dead
	// generation at the same moment it sets this, so a dead generation can
	// never outlive its process or leak a live context, and a later retry
	// never re-closes the same process "for real" twice through the
	// lifecycle: it may only Kill again, which is idempotent in powernap.
	dead atomic.Bool
}

// isUsable reports whether the generation still has a live process behind
// it. A generation that is merely retired (its context is canceled by a
// swap or shutdown) but whose process is still up remains usable for the
// requests already in flight on it; a dead generation must not be used for
// new work.
func (g *clientGeneration) isUsable() bool {
	return !g.dead.Load()
}

func (g *clientGeneration) markRetired() { g.retired.Store(true) }
func (g *clientGeneration) markDead()    { g.dead.Store(true) }
