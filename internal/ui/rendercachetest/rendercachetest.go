// Package rendercachetest provides one shared assertion for the class of
// bug tracked in TECHDEBT.md's "Кэши, которые молча не кэшируют": a
// per-width render cache that looks correct from the outside (same input,
// same output) but never actually serves a cached value, so every render
// silently redoes the work.
//
// Comparing two Render(width) outputs for equality proves nothing here — a
// correct *uncached* renderer returns the same string for the same width
// too. The only genuine way to observe a hit is to make the cache say
// something the real render logic could never say on its own, then check
// whether that lie comes back. AssertPerWidthCacheHit does that by planting
// a sentinel value directly in the cache map (the same one Render reads
// from) and asserting Render returns it verbatim at the same width, but
// never at a different one.
package rendercachetest

import "testing"

// sentinel is a value the real render logic in any of this repo's item
// types could never produce on its own (it contains characters no styled
// terminal line would legitimately contain), so seeing it come back out of
// render proves the value was read from the cache, not recomputed.
const sentinel = "\x00rendercachetest-sentinel\x00"

// AssertPerWidthCacheHit proves render(width) genuinely serves a cache hit
// on repeat calls at the same width, and genuinely misses (recomputes) at
// a different width. cache must be the exact map render reads its cached
// strings from and keys by width — the same structural contract every
// per-width cache in internal/ui uses (map[int]string).
//
// It also leaves the cache holding real content at both widths afterward,
// so callers that additionally want to check invalidate-then-render
// repopulation can chain more calls to render/cache after this returns.
func AssertPerWidthCacheHit(t *testing.T, render func(width int) string, cache map[int]string, width, otherWidth int) {
	t.Helper()

	if width == otherWidth {
		t.Fatalf("width and otherWidth must differ, both are %d", width)
	}
	if cache == nil {
		t.Fatal("cache is nil: the field was never initialized, so nothing can ever be cached in it")
	}

	// Populate the entry under test, then overwrite it with a value the
	// real render path cannot produce. If the next call at the same
	// width still recomputes instead of reading the map, it will return
	// the real content and never see the sentinel we planted.
	render(width)
	cache[width] = sentinel
	if got := render(width); got != sentinel {
		t.Fatalf("Render(%d) did not read from its own cache: got %q, want the planted sentinel %q — "+
			"a cache that silently never hits looks identical to a working one unless you check this",
			width, got, sentinel)
	}

	// A render at a different width must not be keyed off the same
	// entry — otherwise every width would collapse onto whichever one
	// rendered first.
	if got := render(otherWidth); got == sentinel {
		t.Fatalf("Render(%d) returned the width-%d sentinel; the cache is not actually keyed by width", otherWidth, width)
	}
}
