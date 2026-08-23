package dialog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newModelsListTestGroup builds a single group of n model items, matching
// the shape SetGroups produces in the flat list: one header, n items, one
// trailing spacer.
func newModelsListTestGroup(t *testing.T, title string, n int) ModelGroup {
	t.Helper()
	sty := styles.SennitDark()
	g := NewModelGroup(&sty, title)
	for i := range n {
		g.AppendItems(NewModelItem(&sty,
			catwalk.Provider{ID: catwalk.InferenceProvider("codex")},
			catwalk.Model{ID: modelItemTestID(i), Name: modelItemTestID(i)},
			false))
	}
	return g
}

func modelItemTestID(i int) string {
	return "model-" + string(rune('a'+i))
}

// TestModelsListSetSelected_BoundaryIsFlatListLength: SetSelected takes a
// flat-list index (it is handed straight to the embedded List), which also
// counts group headers and spacers, so its bound must be the flat list's
// length rather than the model-item count. Before the fix, an index landing
// on the header or trailing spacer of a later group (in range for the flat
// list but >= the model-item count) skipped the walk-forward-to-a-model-item
// loop entirely and left selection sitting on a non-model item.
func TestModelsListSetSelected_BoundaryIsFlatListLength(t *testing.T) {
	sty := styles.SennitDark()
	l := NewModelsList(&sty, newModelsListTestGroup(t, "Group", 2))
	l.SetGroups(l.groups...)

	// Flat list: [header, item0, item1, spacer] -> indices 0..3.
	require.Equal(t, 4, l.List.Len())
	require.Equal(t, 2, l.Len())

	t.Run("last item is selectable", func(t *testing.T) {
		l.SetSelected(2)
		require.Equal(t, 2, l.Selected())
		item, ok := l.SelectedItem().(*ModelItem)
		require.True(t, ok)
		require.Equal(t, "codex:model-b", item.ID())
	})

	t.Run("one past the end is rejected", func(t *testing.T) {
		l.SetSelected(4)
		require.Equal(t, -1, l.Selected())
	})

	t.Run("far out of bounds is rejected", func(t *testing.T) {
		l.SetSelected(100)
		require.Equal(t, -1, l.Selected())
	})
}

// TestModelsListSetSelected_SkipsHeaderBetweenGroups covers the case that
// motivated the bound fix: an index that lands between groups (on a header
// or spacer, which is >= the model-item count but still within the flat
// list) must walk forward to the next model item instead of being rejected
// outright or left sitting on the non-model item.
func TestModelsListSetSelected_SkipsHeaderBetweenGroups(t *testing.T) {
	sty := styles.SennitDark()
	l := NewModelsList(&sty,
		newModelsListTestGroup(t, "A", 2),
		newModelsListTestGroup(t, "B", 2),
	)
	l.SetGroups(l.groups...)

	// Flat list: [headerA, a0, a1, spacerA, headerB, b0, b1, spacerB]
	// indices:      0       1   2    3         4      5   6    7
	require.Equal(t, 8, l.List.Len())

	l.SetSelected(4) // headerB
	item, ok := l.SelectedItem().(*ModelItem)
	require.True(t, ok, "expected selection to walk forward past the header onto a model item")
	require.Equal(t, "codex:model-a", item.ID()) // first item of group B
}
