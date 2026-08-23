package dialog

import (
	"sort"
	"strings"

	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/sahilm/fuzzy"
)

// ModelsList is a list specifically for model items and groups.
type ModelsList struct {
	*list.List
	groups []ModelGroup
	query  string
	t      *styles.Styles
}

// NewModelsList creates a new list suitable for model items and groups.
func NewModelsList(sty *styles.Styles, groups ...ModelGroup) *ModelsList {
	f := &ModelsList{
		List:   list.NewList(),
		groups: groups,
		t:      sty,
	}
	f.RegisterRenderCallback(list.FocusedRenderCallback(f.List))
	return f
}

// Len returns the number of model items across all groups.
func (f *ModelsList) Len() int {
	n := 0
	for _, g := range f.groups {
		n += len(g.Items)
	}
	return n
}

// SetGroups sets the model groups and updates the list items.
func (f *ModelsList) SetGroups(groups ...ModelGroup) {
	f.groups = groups
	items := []list.Item{}
	for _, g := range f.groups {
		items = append(items, &g)
		for _, item := range g.Items {
			items = append(items, item)
		}
		// Add a space separator after each provider section
		items = append(items, list.NewSpacerItem(1))
	}
	f.SetItems(items...)
}

// SetFilter sets the filter query and updates the list items.
func (f *ModelsList) SetFilter(q string) {
	f.query = q
	f.SetItems(f.VisibleItems()...)
}

// SetSelected sets the selected item index. It overrides the base method to
// skip non-model items.
func (f *ModelsList) SetSelected(index int) {
	// index addresses the flat list (it is handed straight to
	// f.List.SetSelected below), which also holds group headers and
	// spacers, so it must be bounded by the flat list's length rather
	// than f.Len() (the model-item count). Bounding by f.Len() rejected
	// any index landing on a header or trailing spacer before the
	// walk-forward loop below got a chance to skip past it.
	if index < 0 || index >= f.List.Len() {
		f.List.SetSelected(index)
		return
	}

	f.List.SetSelected(index)
	for {
		selectedItem := f.SelectedItem()
		if _, ok := selectedItem.(*ModelItem); ok {
			return
		}
		f.List.SetSelected(index + 1)
		index++
		if index >= f.List.Len() {
			return
		}
	}
}

// SetSelectedItem sets the selected item in the list by item ID.
func (f *ModelsList) SetSelectedItem(itemID string) {
	if itemID == "" {
		return
	}

	// Walk the selectable model items using the same helpers that
	// keyboard navigation uses, so we stay in sync with the flat
	// list layout.
	for ok := f.SelectFirst(); ok; ok = f.SelectNext() {
		if mi, is := f.SelectedItem().(*ModelItem); is && mi.ID() == itemID {
			return
		}
	}
}

// SelectNext selects the next model item, skipping any non-focusable items
// like group headers and spacers.
func (f *ModelsList) SelectNext() (v bool) {
	v = f.List.SelectNext()
	for v {
		selectedItem := f.SelectedItem()
		if _, ok := selectedItem.(*ModelItem); ok {
			return v
		}
		v = f.List.SelectNext()
	}
	return v
}

// SelectPrev selects the previous model item, skipping any non-focusable items
// like group headers and spacers.
func (f *ModelsList) SelectPrev() (v bool) {
	v = f.List.SelectPrev()
	for v {
		selectedItem := f.SelectedItem()
		if _, ok := selectedItem.(*ModelItem); ok {
			return v
		}
		v = f.List.SelectPrev()
	}
	return v
}

// SelectFirst selects the first model item in the list.
func (f *ModelsList) SelectFirst() (v bool) {
	v = f.List.SelectFirst()
	for v {
		selectedItem := f.SelectedItem()
		_, ok := selectedItem.(*ModelItem)
		if ok {
			return v
		}
		v = f.List.SelectNext()
	}
	return v
}

// SelectLast selects the last model item in the list.
func (f *ModelsList) SelectLast() (v bool) {
	v = f.List.SelectLast()
	for v {
		selectedItem := f.SelectedItem()
		if _, ok := selectedItem.(*ModelItem); ok {
			return v
		}
		v = f.List.SelectPrev()
	}
	return v
}

// IsSelectedFirst checks if the selected item is the first model item.
func (f *ModelsList) IsSelectedFirst() bool {
	originalIndex := f.Selected()
	f.SelectFirst()
	isFirst := f.Selected() == originalIndex
	f.List.SetSelected(originalIndex)
	return isFirst
}

// IsSelectedLast checks if the selected item is the last model item.
func (f *ModelsList) IsSelectedLast() bool {
	originalIndex := f.Selected()
	f.SelectLast()
	isLast := f.Selected() == originalIndex
	f.List.SetSelected(originalIndex)
	return isLast
}

// VisibleItems returns the visible items after filtering.
func (f *ModelsList) VisibleItems() []list.Item {
	query := strings.ToLower(strings.ReplaceAll(f.query, " ", ""))

	if query == "" {
		// No filter, return all items with group headers
		items := []list.Item{}
		for _, g := range f.groups {
			items = append(items, &g)
			for _, item := range g.Items {
				item.SetMatch(fuzzy.Match{})
				items = append(items, item)
			}
			// Add a space separator after each provider section
			items = append(items, list.NewSpacerItem(1))
		}
		return items
	}

	// Build one search corpus for every item across every group, each
	// prefixed with its own group's title, and run fuzzy.Find over it
	// once. The previous version called fuzzy.Find per group over the
	// full item set (with every other group's items re-prefixed and
	// then discarded), so rendering N groups meant N full scans of all
	// items; this makes it a single scan regardless of group count.
	type itemInfo struct {
		item      *ModelItem
		groupIdx  int
		prefixLen int
	}
	infos := make([]itemInfo, 0, f.Len())
	names := make([]string, 0, f.Len())
	for gi, g := range f.groups {
		prefix := strings.ToLower(g.Title) + " "
		for _, item := range g.Items {
			infos = append(infos, itemInfo{item: item, groupIdx: gi, prefixLen: len(prefix)})
			names = append(names, prefix+item.Filter())
		}
	}

	matches := fuzzy.Find(query, names)

	// Bucket the matches by group, keyed by the same index into infos/
	// names used above, so each group can be rendered from its own slice
	// without rescanning.
	matchesByGroup := make(map[int][]fuzzy.Match, len(f.groups))
	for _, match := range matches {
		info := infos[match.Index]
		idxs := []int{}
		for _, idx := range match.MatchedIndexes {
			// Adjusts removing provider name highlights
			if idx < info.prefixLen {
				continue
			}
			idxs = append(idxs, idx-info.prefixLen)
		}
		match.MatchedIndexes = idxs
		matchesByGroup[info.groupIdx] = append(matchesByGroup[info.groupIdx], match)
	}

	items := []list.Item{}
	for gi, g := range f.groups {
		groupMatches := matchesByGroup[gi]
		if len(groupMatches) == 0 {
			continue
		}

		// Sort by original index to preserve order within the group
		sort.SliceStable(groupMatches, func(i, j int) bool {
			return groupMatches[i].Index < groupMatches[j].Index
		})

		// Add section header
		items = append(items, &g)
		for _, match := range groupMatches {
			item := infos[match.Index].item
			item.SetMatch(match)
			items = append(items, item)
		}
		// Add a space separator after each provider section
		items = append(items, list.NewSpacerItem(1))
	}

	return items
}

// Render renders the filterable list.
func (f *ModelsList) Render() string {
	return f.List.Render()
}
