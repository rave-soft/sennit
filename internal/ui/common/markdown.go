package common

import (
	"image/color"
	"sync"

	"charm.land/glamour/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/xchroma"
)

// formatterName is also used as the chroma style registry name in
// chromastyle.go: both registries are keyed by the same brand slug, so one
// constant covers both.
const formatterName = brand.Slug

func init() {
	// NOTE: Glamour does not offer us an option to pass the formatter
	// implementation directly. We need to register and use by name.
	var zero color.Color
	formatters.Register(formatterName, xchroma.Formatter(zero, nil))
}

// mdCacheMu guards mdCache, quietMDCache and mdCacheRev.
var (
	mdCacheMu sync.Mutex
	// mdCacheRev is the palette build the cached renderers were made
	// from. A glamour renderer bakes in the style config it was built
	// with, so a width-only key would keep rendering markdown in the
	// previous palette after a theme switch. Both caches are dropped
	// wholesale when the revision moves.
	mdCacheRev   uint64
	mdCache      = map[int]*glamour.TermRenderer{}
	quietMDCache = map[int]*glamour.TermRenderer{}
)

// resetMDCachesForRev drops both renderer caches when sty comes from a
// different palette build than the one they were populated for. Callers
// must hold mdCacheMu.
func resetMDCachesForRev(sty *styles.Styles) {
	rev := sty.Rev()
	if rev == mdCacheRev {
		return
	}
	mdCacheRev = rev

	// The renderers we're about to drop are also keyed into
	// rendererLocks by pointer (see LockMarkdownRenderer). A map key
	// holds a strong reference, so leaving those entries behind would
	// keep every renderer built under every past theme alive for the
	// life of the process. Collect them before clearing the caches and
	// evict them from rendererLocks too.
	stale := make([]*glamour.TermRenderer, 0, len(mdCache)+len(quietMDCache))
	for _, r := range mdCache {
		stale = append(stale, r)
	}
	for _, r := range quietMDCache {
		stale = append(stale, r)
	}
	clear(mdCache)
	clear(quietMDCache)

	if len(stale) > 0 {
		rendererLocksMu.Lock()
		for _, r := range stale {
			delete(rendererLocks, r)
		}
		rendererLocksMu.Unlock()
	}
}

// MarkdownRenderer returns a glamour [glamour.TermRenderer] configured with
// the given styles and width. Renderers are memoized per width (and
// palette build, see resetMDCachesForRev) and shared across callers.
//
// The returned renderer is NOT safe for concurrent Render calls
// (goldmark's BlockStack carries state across the public Render
// API). Sennit's TUI is single-threaded so production never
// contends, but parallel callers (most notably parallel tests)
// must serialize via [LockMarkdownRenderer]. Treat the renderer
// as effectively pinned to one goroutine at a time.
func MarkdownRenderer(sty *styles.Styles, width int) *glamour.TermRenderer {
	mdCacheMu.Lock()
	defer mdCacheMu.Unlock()
	resetMDCachesForRev(sty)
	if r, ok := mdCache[width]; ok {
		return r
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(sty.Markdown),
		glamour.WithWordWrap(width),
		glamour.WithChromaFormatter(formatterName),
	)
	if err != nil {
		// Don't cache a failed build: caching nil here would make
		// every later call at this width return nil forever (a
		// guaranteed nil-deref for callers), instead of retrying on
		// the next call the way an uncached miss does.
		return nil
	}
	mdCache[width] = r
	return r
}

// QuietMarkdownRenderer returns a glamour [glamour.TermRenderer] with no colors
// (plain text with structure) and the given width. Renderers are memoized per
// width (and palette build) and shared across callers. Same concurrency contract as
// [MarkdownRenderer]: serialize via [LockMarkdownRenderer].
func QuietMarkdownRenderer(sty *styles.Styles, width int) *glamour.TermRenderer {
	mdCacheMu.Lock()
	defer mdCacheMu.Unlock()
	resetMDCachesForRev(sty)
	if r, ok := quietMDCache[width]; ok {
		return r
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(sty.QuietMarkdown),
		glamour.WithWordWrap(width),
		glamour.WithChromaFormatter(formatterName),
	)
	if err != nil {
		// See the matching comment in MarkdownRenderer: don't cache a
		// failed build under this width, or every later call would
		// return the cached nil forever instead of retrying.
		return nil
	}
	quietMDCache[width] = r
	return r
}

// rendererLocksMu guards rendererLocks. We key per-renderer
// mutexes by pointer so the lock granularity matches the
// renderer cache granularity (one mutex per (width, palette)
// renderer instance, not one mutex for the entire cache).
var (
	rendererLocksMu sync.Mutex
	rendererLocks   = map[*glamour.TermRenderer]*sync.Mutex{}
)

// LockMarkdownRenderer returns the per-renderer mutex used to
// serialize concurrent Render calls on a shared
// [glamour.TermRenderer] instance. The returned [*sync.Mutex] is
// stable for the lifetime of the renderer.
//
// Callers that issue more than one Render call in the same
// logical operation should hold the mutex for the entire
// sequence so other goroutines do not interleave their own
// Render calls and corrupt the renderer state. F8's
// streamingMarkdown is the immediate consumer; other call
// sites that today issue exactly one Render call per item
// render are safe without locking under the single-threaded
// TUI Update loop, but should adopt this lock if they ever run
// in parallel (e.g. background prerender workers).
func LockMarkdownRenderer(r *glamour.TermRenderer) *sync.Mutex {
	rendererLocksMu.Lock()
	defer rendererLocksMu.Unlock()
	if mu, ok := rendererLocks[r]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	rendererLocks[r] = mu
	return mu
}
