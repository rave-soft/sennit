package list

import (
	"testing"

	"github.com/sahilm/fuzzy"
)

func TestBaseItemLifecycle(t *testing.T) {
	item := NewBaseItem()
	if !item.Finished() || item.Version() != 0 || item.Cache() == nil {
		t.Fatal("new BaseItem must be finished with version zero and an empty cache")
	}

	item.Cache()[80] = "cached"
	item.SetFocused(false)
	if item.Version() != 0 || item.Cache()[80] != "cached" {
		t.Fatal("a no-op focus change must preserve version and cache")
	}

	item.SetFocused(true)
	if !item.Focused() || item.Version() != 1 || item.Cache() != nil {
		t.Fatal("a focus change must clear cache and bump version")
	}

	match := fuzzy.Match{Str: "item", MatchedIndexes: []int{0, 2}}
	item.SetMatch(match)
	if item.Version() != 2 || item.Match().Str != "item" {
		t.Fatal("a match change must bump version and retain the match")
	}
	item.SetMatch(fuzzy.Match{Str: "item", MatchedIndexes: []int{0, 2}})
	if item.Version() != 2 {
		t.Fatal("an equivalent match must not bump version")
	}

	item.Invalidate()
	if item.Version() != 3 || item.Cache() != nil {
		t.Fatal("explicit invalidation must clear cache and bump version")
	}
}

// TestNewSpacerItem covers the height-minus-one bookkeeping:
// NewSpacerItem stores Height-1 (clamped at zero) since the list
// itself supplies one implicit blank row via the item boundary, and
// Render/Finished expose that stored height directly.
func TestNewSpacerItem(t *testing.T) {
	t.Parallel()

	s := NewSpacerItem(3)
	if s.Height != 2 {
		t.Fatalf("NewSpacerItem(3).Height = %d, want 2", s.Height)
	}
	if got, want := s.Render(80), "\n\n"; got != want {
		t.Fatalf("Render(80) = %q, want %q", got, want)
	}
	if !s.Finished() {
		t.Fatal("SpacerItem must always report Finished")
	}
	if s.Version() != 0 {
		t.Fatalf("fresh SpacerItem must start at version 0, got %d", s.Version())
	}
}

// TestNewSpacerItem_ClampsAtZero covers the max(0, height-1) clamp:
// a height of 0 or 1 must never produce a negative repeat count.
func TestNewSpacerItem_ClampsAtZero(t *testing.T) {
	t.Parallel()

	for _, h := range []int{0, 1, -5} {
		s := NewSpacerItem(h)
		if s.Height != 0 {
			t.Fatalf("NewSpacerItem(%d).Height = %d, want 0", h, s.Height)
		}
		if got := s.Render(80); got != "" {
			t.Fatalf("NewSpacerItem(%d).Render(80) = %q, want empty", h, got)
		}
	}
}
