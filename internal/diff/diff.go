package diff

import (
	"log"
	"strings"

	"github.com/aymanbagabas/go-udiff"
)

// GenerateDiff creates a unified diff from two file contents.
func GenerateDiff(beforeContent, afterContent, fileName string) (string, int, int) {
	fileName = strings.TrimPrefix(fileName, "/")

	// Count from the structured hunks udiff computed rather than
	// re-parsing the rendered text: a content line that itself starts
	// with "+++" or "---" (a markdown fence, a git conflict marker, a
	// "--" SQL comment...) is indistinguishable from a diff header once
	// it has been rendered to text, so prefix-matching the string
	// under-counts. ToUnifiedDiff and Unified render from the same
	// edits, so the string returned here is identical to udiff.Unified.
	edits := udiff.Lines(beforeContent, afterContent)
	unified, err := udiff.ToUnifiedDiff("a/"+fileName, "b/"+fileName, beforeContent, edits, udiff.DefaultContextLines)
	if err != nil {
		// Can't happen: edits come straight from udiff.Lines.
		log.Fatalf("internal error in diff.GenerateDiff: %v", err)
	}

	additions, removals := 0, 0
	for _, hunk := range unified.Hunks {
		for _, line := range hunk.Lines {
			switch line.Kind {
			case udiff.Insert:
				additions++
			case udiff.Delete:
				removals++
			}
		}
	}

	return unified.String(), additions, removals
}
