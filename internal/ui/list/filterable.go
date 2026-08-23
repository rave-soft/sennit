package list

import (
	"github.com/sahilm/fuzzy"
)

// FilterableItem is an item that can be filtered via a query.
type FilterableItem interface {
	Item
	// Filter returns the value to be used for filtering.
	Filter() string
}

// MatchSettable is an interface for items that can have their match indexes
// and match score set.
type MatchSettable interface {
	SetMatch(fuzzy.Match)
}

// FilterableList is a list that takes filterable items that can be filtered
// via a settable query.
type FilterableList struct {
	*List
	items []FilterableItem
	query string

	// itemsGen bumps whenever items is replaced wholesale (SetItems /
	// AppendItems / PrependItems). Together with query it's the cache key
	// below — cheap to compare, unlike diffing the items slice itself.
	itemsGen int

	// filtered memoizes the last FilteredItems computation, valid as long
	// as filteredQuery/filteredGen still match query/itemsGen. Both
	// FilteredItems (called several times per frame by the commands
	// dialog) and Render hit this instead of re-running the fuzzy filter
	// and rebuilding the result slice on every call.
	filtered      []Item
	filteredValid bool
	filteredQuery string
	filteredGen   int

	// listSynced reports whether the embedded List's own items already
	// reflect `filtered`. Render skips the embedded SetItems call (which
	// resets scroll offset bookkeeping and invalidates the cached total
	// height — real work, not a no-op) whenever nothing has changed since
	// the last time it ran.
	listSynced bool
}

// NewFilterableList creates a new filterable list.
func NewFilterableList(items ...FilterableItem) *FilterableList {
	f := &FilterableList{
		List:  NewList(),
		items: items,
	}
	f.RegisterRenderCallback(FocusedRenderCallback(f.List))
	f.SetItems(items...)
	return f
}

// SetItems sets the list items and updates the filtered items.
func (f *FilterableList) SetItems(items ...FilterableItem) {
	f.items = items
	f.itemsGen++
	fitems := make([]Item, len(items))
	for i, item := range items {
		fitems[i] = item
	}
	f.List.SetItems(fitems...)
	// The embedded list above was just synced with the unfiltered items,
	// not the filtered set (matching the pre-caching behavior, which
	// always resynced on the very next Render call regardless). Mark it
	// unsynced so that resync still happens.
	f.listSynced = false
}

// AppendItems appends items to the list and updates the filtered items.
func (f *FilterableList) AppendItems(items ...FilterableItem) {
	f.items = append(f.items, items...)
	f.itemsGen++
	itms := make([]Item, len(f.items))
	for i, item := range f.items {
		itms[i] = item
	}
	f.List.SetItems(itms...)
	f.listSynced = false
}

// PrependItems prepends items to the list and updates the filtered items.
func (f *FilterableList) PrependItems(items ...FilterableItem) {
	f.items = append(items, f.items...)
	f.itemsGen++
	itms := make([]Item, len(f.items))
	for i, item := range f.items {
		itms[i] = item
	}
	f.List.SetItems(itms...)
	f.listSynced = false
}

// SetFilter sets the filter query and updates the list items.
func (f *FilterableList) SetFilter(q string) {
	f.query = q
	f.List.SetItems(f.FilteredItems()...)
	f.listSynced = true
	f.ScrollToTop()
}

// InvalidateAll drops every cached render, including those of items the
// current filter hides — the embedded list only knows about the visible
// ones, and a hidden item with a stale cache would come back in the old
// palette the moment the filter changed.
func (f *FilterableList) InvalidateAll() {
	for _, item := range f.items {
		if inv, ok := item.(interface{ Invalidate() }); ok {
			inv.Invalidate()
		}
	}
	f.List.InvalidateAll()
}

// FilterableItemsSource is a type that implements [fuzzy.Source] for filtering
// [FilterableItem]s.
type FilterableItemsSource []FilterableItem

// Len returns the length of the source.
func (f FilterableItemsSource) Len() int {
	return len(f)
}

// String returns the string representation of the item at index i.
func (f FilterableItemsSource) String(i int) string {
	return f[i].Filter()
}

// FilteredItems returns the visible items after filtering. The result is
// memoized against the query and item set it was computed from (see
// itemsGen/filteredGen): a query that hasn't changed since the last call,
// against the same items, returns the cached slice instead of re-running
// the fuzzy filter — this is called several times per frame from the
// commands dialog, plus once more from Render below.
func (f *FilterableList) FilteredItems() []Item {
	if f.filteredValid && f.filteredQuery == f.query && f.filteredGen == f.itemsGen {
		return f.filtered
	}
	items := f.computeFilteredItems()
	f.filtered = items
	f.filteredValid = true
	f.filteredQuery = f.query
	f.filteredGen = f.itemsGen
	// The cached filtered set just changed, so whatever the embedded List
	// currently holds (from before this recompute) is stale.
	f.listSynced = false
	return items
}

// computeFilteredItems runs the fuzzy filter (or, for an empty query,
// clears every item's match state) and builds the visible-items slice.
// Split out from FilteredItems so the memoization above wraps it cleanly.
func (f *FilterableList) computeFilteredItems() []Item {
	if f.query == "" {
		items := make([]Item, len(f.items))
		for i, item := range f.items {
			if ms, ok := item.(MatchSettable); ok {
				ms.SetMatch(fuzzy.Match{})
				item = ms.(FilterableItem)
			}
			items[i] = item
		}
		return items
	}

	items := FilterableItemsSource(f.items)
	matches := fuzzy.FindFrom(f.query, items)
	matchedItems := []Item{}
	resultSize := len(matches)
	for i := range resultSize {
		match := matches[i]
		item := items[match.Index]
		if ms, ok := item.(MatchSettable); ok {
			ms.SetMatch(match)
			item = ms.(FilterableItem)
		}
		matchedItems = append(matchedItems, item)
	}

	return matchedItems
}

// Render renders the filterable list. The embedded List's SetItems (which
// resets scroll bookkeeping and invalidates the cached total height) only
// runs when the filtered set actually changed since the last Render —
// see listSynced.
func (f *FilterableList) Render() string {
	items := f.FilteredItems()
	if !f.listSynced {
		f.List.SetItems(items...)
		f.listSynced = true
	}
	return f.List.Render()
}
