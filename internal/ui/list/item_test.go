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
