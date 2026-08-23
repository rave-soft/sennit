package list

import (
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/sahilm/fuzzy"
)

// Item represents a single item in the lazy-loaded list.
//
// Items participate in the list-level render memo (F6). The cache key
// for each item is (pointer, width, version). Items must:
//
//   - Bump their version (via the embedded *Versioned helper) on every
//     mutation that changes the rendered output.
//   - Return Finished() == true once their rendered output will not
//     change again unless an explicit mutator is invoked. Frozen
//     entries are emitted verbatim — no Render call — until either
//     Version() bumps, the viewport width changes, or the list
//     explicitly invalidates the entry.
type Item interface {
	// Render returns the string representation of the item for the given
	// width.
	Render(width int) string

	// Version returns a monotonic counter that the list-level cache
	// uses to detect mutations. Items must increment the version
	// (via Versioned.Bump) on every state change that would alter
	// the rendered output.
	Version() uint64

	// Finished reports whether the item's rendered output has reached
	// a terminal state and may be frozen by the list cache. Items
	// that animate, stream, or otherwise still mutate must return
	// false. A finished item that later mutates must bump its
	// version on the mutation; the cache treats version bumps as
	// implicit unfreeze + invalidate.
	Finished() bool
}

// Versioned is a tiny embeddable helper that satisfies Item.Version()
// and provides a Bump() method to call from every state-mutating
// method. Items typically embed *Versioned alongside their other
// helpers; see chat.AssistantMessageItem for the canonical wiring.
//
// Bump() is not safe for concurrent use; callers must hold whatever
// synchronization their item type already requires for state
// mutations. The list itself never reads Version() from a goroutine
// other than the UI thread.
type Versioned struct {
	v uint64
}

// NewVersioned returns a fresh *Versioned at version zero.
func NewVersioned() *Versioned {
	return &Versioned{}
}

// Version returns the current version counter.
func (vc *Versioned) Version() uint64 {
	return vc.v
}

// Bump advances the version counter by one. Mutators on items that
// affect the rendered output must call Bump exactly once per
// observable state change. Bumping more than once per change is
// harmless other than a single extra cache miss.
func (vc *Versioned) Bump() {
	vc.v++
}

// BaseItem provides the common lifecycle state for stable, filterable list
// items. Embed it in an item and use SetFocused/SetMatch from the embedded
// value; observable changes clear the per-width cache and bump Version.
type BaseItem struct {
	*Versioned
	cache   map[int]string
	focused bool
	match   fuzzy.Match
}

// NewBaseItem creates an item lifecycle helper with a fresh version.
func NewBaseItem() BaseItem {
	return BaseItem{Versioned: NewVersioned(), cache: make(map[int]string)}
}

// Finished implements Item for static list rows.
func (b *BaseItem) Finished() bool { return true }

// SetFocused updates focus only when it changes.
func (b *BaseItem) SetFocused(focused bool) {
	if b.focused == focused {
		return
	}
	b.focused = focused
	b.invalidate()
}

// SetMatch updates the fuzzy match only when it changes.
func (b *BaseItem) SetMatch(match fuzzy.Match) {
	if sameMatch(b.match, match) {
		return
	}
	b.match = match
	b.invalidate()
}

// Focused reports the current focus state.
func (b *BaseItem) Focused() bool { return b.focused }

// Match reports the current fuzzy match.
func (b *BaseItem) Match() *fuzzy.Match { return &b.match }

// Cache returns the item's per-width render cache.
func (b *BaseItem) Cache() map[int]string { return b.cache }

// Invalidate clears cached renderings and advances the item version.
func (b *BaseItem) Invalidate() { b.invalidate() }

func (b *BaseItem) invalidate() {
	// Clear in place rather than assigning nil: renderItem's nil-guard
	// (in the dialog package) only ever populates a throwaway local map
	// when the field it's handed is nil, so setting it to nil here would
	// silently disable the per-width cache for the rest of the item's
	// life instead of just dropping the stale entries.
	clear(b.cache)
	if b.Versioned == nil {
		b.Versioned = NewVersioned()
	}
	b.Bump()
}

func sameMatch(a, b fuzzy.Match) bool {
	return a.Str == b.Str && a.Index == b.Index && a.Score == b.Score &&
		slices.Equal(a.MatchedIndexes, b.MatchedIndexes)
}

// RawRenderable represents an item that can provide a raw rendering
// without additional styling.
type RawRenderable interface {
	// RawRender returns the raw rendered string without any additional
	// styling.
	RawRender(width int) string
}

// Focusable represents an item that can be aware of focus state changes.
type Focusable interface {
	// SetFocused sets the focus state of the item.
	SetFocused(focused bool)
}

// Highlightable represents an item that can highlight a portion of its content.
type Highlightable interface {
	// SetHighlight highlights the content from the given start to end
	// positions. Use -1 for no highlight.
	SetHighlight(startLine, startCol, endLine, endCol int)
	// Highlight returns the current highlight positions within the item.
	Highlight() (startLine, startCol, endLine, endCol int)
}

// MouseClickable represents an item that can handle mouse click events.
type MouseClickable interface {
	// HandleMouseClick processes a mouse click event at the given coordinates.
	// It returns true if the event was handled, false otherwise.
	HandleMouseClick(btn ansi.MouseButton, x, y int) bool
}

// AlwaysSpaced opts an item out of dense neighbor grouping regardless of
// its rendered height. The list's inter-item gap normally collapses to
// zero between two adjacent items that both render on a single line
// (see List.gapAt), so a run of one-line tool calls reads as a tight
// column instead of a wall of blank lines. Items that implement
// AlwaysSpaced and return true keep the full gap on both sides even
// when they happen to render as one line — e.g. chat/assistant and
// chat/user text, and Agent/AgenticFetch delegation blocks, which are
// meaningful visual boundaries regardless of their current height.
type AlwaysSpaced interface {
	AlwaysSpaced() bool
}

// SpacerItem is a spacer item that adds vertical space in the list.
type SpacerItem struct {
	*Versioned
	Height int
}

// NewSpacerItem creates a new [SpacerItem] with the specified height.
func NewSpacerItem(height int) *SpacerItem {
	return &SpacerItem{
		Versioned: NewVersioned(),
		Height:    max(0, height-1),
	}
}

// Render implements the Item interface for [SpacerItem].
func (s *SpacerItem) Render(width int) string {
	return strings.Repeat("\n", s.Height)
}

// Finished implements Item. SpacerItems are immutable in practice and
// safe to freeze; any mutation goes through Versioned.Bump which
// invalidates the frozen entry.
func (s *SpacerItem) Finished() bool {
	return true
}
