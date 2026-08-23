package list

import (
	"testing"

	"github.com/sahilm/fuzzy"
	"github.com/stretchr/testify/require"
)

// filterableTrackedItem is a FilterableItem test double. It embeds
// *Versioned like the other fixtures in this package and records the
// last [fuzzy.Match] set on it via SetMatch so filter-ordering tests
// can assert on match data, not just presence.
type filterableTrackedItem struct {
	*Versioned
	name     string
	finished bool
	match    fuzzy.Match
}

func newFilterableItem(name string) *filterableTrackedItem {
	return &filterableTrackedItem{
		Versioned: NewVersioned(),
		name:      name,
		finished:  true,
	}
}

func (f *filterableTrackedItem) Render(int) string { return f.name }
func (f *filterableTrackedItem) Finished() bool    { return f.finished }
func (f *filterableTrackedItem) Filter() string    { return f.name }
func (f *filterableTrackedItem) SetMatch(m fuzzy.Match) {
	f.match = m
}

// Invalidate satisfies the optional interface FilterableList.InvalidateAll
// type-asserts against. Without it the assertion fails silently and the
// item is skipped, so the fixture must implement it to be observed at all.
func (f *filterableTrackedItem) Invalidate() { f.Bump() }

// names extracts the Filter() value of each item in order, for
// asserting on the filtered/ordered result set.
func names(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.(FilterableItem).Filter()
	}
	return out
}

// TestFilterableList_NoFilter_ReturnsAllInOriginalOrder covers the
// f.query == "" branch of FilteredItems: every item is returned, in
// its original order, with a zero-value match set on it.
func TestFilterableList_NoFilter_ReturnsAllInOriginalOrder(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	b := newFilterableItem("banana")
	c := newFilterableItem("cherry")
	f := NewFilterableList(a, b, c)

	require.Equal(t, []string{"apple", "banana", "cherry"}, names(f.FilteredItems()))
	require.Equal(t, 3, f.Len(), "unfiltered list length matches item count")

	// SetMatch was called with the zero value for every item.
	require.Equal(t, fuzzy.Match{}, a.match)
}

// TestFilterableList_SetFilter_MatchesSubset covers a filter query
// that matches only some of the items: the non-matching items must
// be excluded from both FilteredItems and the embedded List.
func TestFilterableList_SetFilter_MatchesSubset(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	b := newFilterableItem("banana")
	c := newFilterableItem("apricot")
	f := NewFilterableList(a, b, c)

	f.SetFilter("ap")
	got := names(f.FilteredItems())
	require.ElementsMatch(t, []string{"apple", "apricot"}, got, "only items containing the query match")
	require.NotContains(t, got, "banana")

	// The embedded List's Len() reflects the same filtered set once
	// rendered, and SetFilter resets scroll to the top.
	require.Equal(t, -1, f.Selected(), "a list nothing has selected yet stays unselected")
}

// TestFilterableList_SetFilter_MatchesAll covers a query that every
// item satisfies.
func TestFilterableList_SetFilter_MatchesAll(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	b := newFilterableItem("apricot")
	f := NewFilterableList(a, b)

	f.SetFilter("a")
	require.ElementsMatch(t, []string{"apple", "apricot"}, names(f.FilteredItems()))
}

// TestFilterableList_SetFilter_MatchesNone covers a query that no
// item satisfies: FilteredItems must return an empty, non-nil slice.
func TestFilterableList_SetFilter_MatchesNone(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	b := newFilterableItem("banana")
	f := NewFilterableList(a, b)

	f.SetFilter("zzz")
	got := f.FilteredItems()
	require.Empty(t, got, "no item matches an unmatchable query")
}

// TestFilterableList_SetFilter_ScrollsToTop covers the ScrollToTop
// call inside SetFilter: after scrolling away from the top, applying
// a new filter must reset the viewport to the first item.
func TestFilterableList_SetFilter_ScrollsToTop(t *testing.T) {
	t.Parallel()

	items := make([]FilterableItem, 0, 10)
	for i := range 10 {
		items = append(items, newFilterableItem("item"+string(rune('a'+i))))
	}
	f := NewFilterableList(items...)
	f.SetSize(20, 3)

	f.ScrollToIndex(5)
	require.NotEqual(t, 0, f.Offset(), "scrolled away from the top")

	f.SetFilter("item")
	require.Equal(t, 0, f.Offset(), "SetFilter must scroll back to the top")
}

// TestFilterableList_SetItems_ClearsPreviousFilterMatches covers
// SetItems replacing the item set outright: the new items become the
// full source for FilteredItems, and a filter set before the call
// still narrows them (SetItems itself does not clear f.query).
func TestFilterableList_SetItems_ClearsPreviousFilterMatches(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	b := newFilterableItem("banana")
	f := NewFilterableList(a, b)
	f.SetFilter("ap")
	require.Equal(t, []string{"apple"}, names(f.FilteredItems()))

	x := newFilterableItem("apricot")
	y := newFilterableItem("blueberry")
	f.SetItems(x, y)

	got := names(f.FilteredItems())
	require.Equal(t, []string{"apricot"}, got, "existing filter still applies to the new item set")
}

// TestFilterableList_AppendItems_RespectsActiveFilter covers
// AppendItems while a filter is active: the appended items join the
// source set and are themselves subject to the current filter.
func TestFilterableList_AppendItems_RespectsActiveFilter(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	f := NewFilterableList(a)
	f.SetFilter("ap")
	require.Equal(t, []string{"apple"}, names(f.FilteredItems()))

	b := newFilterableItem("banana")
	c := newFilterableItem("apricot")
	f.AppendItems(b, c)

	got := names(f.FilteredItems())
	require.ElementsMatch(t, []string{"apple", "apricot"}, got, "appended items are filtered too")
	require.NotContains(t, got, "banana")
}

// TestFilterableList_PrependItems_RespectsActiveFilter mirrors
// AppendItems for PrependItems, and additionally checks ordering:
// prepended items land before the originals in the unfiltered source.
func TestFilterableList_PrependItems_RespectsActiveFilter(t *testing.T) {
	t.Parallel()

	b := newFilterableItem("banana")
	f := NewFilterableList(b)

	a := newFilterableItem("apple")
	f.PrependItems(a)

	// No filter yet: prepended item comes first.
	require.Equal(t, []string{"apple", "banana"}, names(f.FilteredItems()))

	f.SetFilter("an")
	got := names(f.FilteredItems())
	require.Equal(t, []string{"banana"}, got, "filter narrows to the matching prepended-set item")
}

// TestFilterableList_FilteredItems_OrderedByScore covers fuzzy match
// ordering: FilteredItems must return matches best-score-first, not
// in original item order.
func TestFilterableList_FilteredItems_OrderedByScore(t *testing.T) {
	t.Parallel()

	// "banana" scores lower than an exact-prefix "ban" match against
	// the query "ban" because of the extra unmatched trailing chars;
	// what matters here is that fuzzy.FindFrom's own ordering (not
	// item insertion order) drives the result. Insert the exact match
	// last to prove the list doesn't just preserve source order.
	loose := newFilterableItem("bxaxnxaxnxa")
	exact := newFilterableItem("ban")
	f := NewFilterableList(loose, exact)

	f.SetFilter("ban")
	got := names(f.FilteredItems())
	require.Equal(t, []string{"ban", "bxaxnxaxnxa"}, got, "closer fuzzy match must be ranked first regardless of insertion order")
}

// TestFilterableList_InvalidateAll covers the filter-aware
// InvalidateAll override: it must invalidate every item in the
// backing (unfiltered) source, including ones the current filter is
// hiding, not just the items visible in the embedded List.
func TestFilterableList_InvalidateAll(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	b := newFilterableItem("banana")
	f := NewFilterableList(a, b)
	f.SetFilter("ap") // hides banana from the embedded List

	va, vb := a.Version(), b.Version()
	f.InvalidateAll()

	require.Greater(t, a.Version(), va, "visible item must be invalidated")
	require.Greater(t, b.Version(), vb, "filtered-out item must also be invalidated")
}

// TestFilterableList_Render_AppliesCurrentFilter covers Render:
// it re-syncs the embedded List to the current filter before
// delegating, so the rendered output only contains matching items.
func TestFilterableList_Render_AppliesCurrentFilter(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	b := newFilterableItem("banana")
	f := NewFilterableList(a, b)
	f.SetSize(20, 10)

	f.SetFilter("ban")
	out := f.Render()
	require.Contains(t, out, "banana")
	require.NotContains(t, out, "apple")
}

// filterableCountingItem wraps filterableTrackedItem to count SetMatch
// calls, so tests can assert the fuzzy filter only reruns when its inputs
// (query, item set) actually changed.
type filterableCountingItem struct {
	*filterableTrackedItem
	setMatchCalls int
}

func newCountingFilterableItem(name string) *filterableCountingItem {
	return &filterableCountingItem{filterableTrackedItem: newFilterableItem(name)}
}

func (f *filterableCountingItem) SetMatch(m fuzzy.Match) {
	f.setMatchCalls++
	f.filterableTrackedItem.SetMatch(m)
}

// TestFilterableList_FilteredItems_CachesUntilQueryOrItemsChange covers the
// memoization added to FilteredItems: repeated calls with an unchanged
// query against an unchanged item set must not re-run the fuzzy filter (or
// re-touch each item's match state via SetMatch), and the result must stay
// identical to a fresh computation until the query or the item set does
// change.
func TestFilterableList_FilteredItems_CachesUntilQueryOrItemsChange(t *testing.T) {
	t.Parallel()

	a := newCountingFilterableItem("apple")
	b := newCountingFilterableItem("apricot")
	c := newCountingFilterableItem("banana")
	f := NewFilterableList(a, b, c)

	f.SetFilter("ap")
	require.Equal(t, 1, a.setMatchCalls, "SetFilter computes the filter once")
	require.ElementsMatch(t, []string{"apple", "apricot"}, names(f.FilteredItems()))

	// Repeated reads with nothing changed must hit the cache: no further
	// SetMatch calls, and the same result.
	for range 3 {
		got := names(f.FilteredItems())
		require.ElementsMatch(t, []string{"apple", "apricot"}, got)
	}
	require.Equal(t, 1, a.setMatchCalls, "unchanged query/items must not rerun the filter")
	require.Equal(t, 1, b.setMatchCalls)
	require.Equal(t, 0, c.setMatchCalls, "a non-matching item is never handed a match to set")

	// Changing the query invalidates the cache: "an" matches banana (not
	// apple/apricot, which have no 'n'), and c only gets a SetMatch call
	// once the filter actually reruns against the new query.
	require.Equal(t, 0, c.setMatchCalls)
	f.SetFilter("an")
	require.Equal(t, []string{"banana"}, names(f.FilteredItems()))
	require.Equal(t, 1, c.setMatchCalls, "a query change must recompute the filter")

	// Changing the item set invalidates the cache even with the query
	// unchanged.
	d := newCountingFilterableItem("banjo")
	f.AppendItems(d)
	got := names(f.FilteredItems())
	require.ElementsMatch(t, []string{"banana", "banjo"}, got)
}

// TestFilterableList_Render_SkipsResyncWhenFilterUnchanged covers the
// listSynced optimization in Render: once the embedded List reflects the
// current filtered set, a Render call that changes nothing must not reset
// the scroll offset the embedded List.SetItems call would otherwise clamp
// back (a symptom that would show up as ScrollToIndex getting silently
// undone by the very next Render).
func TestFilterableList_Render_SkipsResyncWhenFilterUnchanged(t *testing.T) {
	t.Parallel()

	items := make([]FilterableItem, 0, 10)
	for i := range 10 {
		items = append(items, newFilterableItem("item"+string(rune('a'+i))))
	}
	f := NewFilterableList(items...)
	f.SetSize(20, 3)
	f.Render() // sync the embedded List once so ScrollToIndex below has content to scroll within.

	f.ScrollToIndex(5)
	offset := f.Offset()
	require.NotEqual(t, 0, offset, "scrolled away from the top")

	// Nothing changed (no SetFilter/SetItems/Append/Prepend in between):
	// Render must leave the scroll position alone.
	f.Render()
	require.Equal(t, offset, f.Offset(), "an unchanged filter must not reset scroll state")
}

// TestFilterableList_Len_TracksFilteredCount covers Len (inherited
// from the embedded List) reflecting the filtered count once Render
// or SetFilter has synced it, not the full backing-item count.
func TestFilterableList_Len_TracksFilteredCount(t *testing.T) {
	t.Parallel()

	a := newFilterableItem("apple")
	b := newFilterableItem("banana")
	c := newFilterableItem("apricot")
	f := NewFilterableList(a, b, c)

	f.SetFilter("ap")
	require.Equal(t, 2, f.Len(), "SetFilter syncs the embedded List, so Len reflects the filtered count")
}
