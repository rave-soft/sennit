package filetracker

import (
	"encoding/json"
	"log/slog"
	"slices"
)

// LineRange is an inclusive, 1-based span of lines.
type LineRange struct {
	Start int
	End   int
}

// Coverage is how much of a file a session has actually seen. Full means
// the whole file was read and Ranges is irrelevant; otherwise Ranges holds
// the merged, sorted spans that were.
//
// The distinction matters because the read tool serves windows: a read of
// lines 1-50 of a 2000-line file used to record the same "this file was
// read" fact as a read of all of it, which let an edit land on line 1900
// with nothing to check it against.
type Coverage struct {
	Full   bool
	Ranges []LineRange
}

// FullCoverage is the coverage of a file that was read end to end.
var FullCoverage = Coverage{Full: true}

// Covers reports whether every line in [start, end] was read. A file never
// read at all has no coverage; a fully read one covers everything.
//
// A span that runs past the end of the recorded ranges is not covered:
// lines can only have been read if they were in a window that was served.
func (c Coverage) Covers(start, end int) bool {
	if c.Full {
		return true
	}
	if start > end {
		start, end = end, start
	}
	for _, r := range c.Ranges {
		if r.Start > start {
			// Ranges are sorted and merged, so a gap here means the
			// remaining ranges start even later.
			return false
		}
		if r.End >= end {
			return true
		}
		if r.End >= start {
			// Partially covered: the tail continues past this range, and
			// a merged range list has a gap after it by construction.
			return false
		}
	}
	return false
}

// Empty reports whether nothing at all has been read.
func (c Coverage) Empty() bool {
	return !c.Full && len(c.Ranges) == 0
}

// Add returns the coverage with r merged in. Adjacent and overlapping
// ranges are fused, so reading lines 1-50 and then 51-100 yields a single
// 1-100 span and an edit spanning that boundary is allowed.
func (c Coverage) Add(r LineRange) Coverage {
	if c.Full {
		return c
	}
	if r.Start > r.End {
		r.Start, r.End = r.End, r.Start
	}
	if r.Start < 1 {
		r.Start = 1
	}

	merged := append(slices.Clone(c.Ranges), r)
	slices.SortFunc(merged, func(a, b LineRange) int { return a.Start - b.Start })

	out := merged[:1]
	for _, next := range merged[1:] {
		last := &out[len(out)-1]
		if next.Start <= last.End+1 {
			last.End = max(last.End, next.End)
			continue
		}
		out = append(out, next)
	}
	return Coverage{Ranges: out}
}

// encodeRanges serializes coverage for storage: the empty string for a
// full read (which is also what rows written before ranges existed hold,
// so old sessions read back as fully covered rather than as unreadable).
func encodeRanges(c Coverage) string {
	if c.Full || len(c.Ranges) == 0 {
		return ""
	}
	pairs := make([][2]int, 0, len(c.Ranges))
	for _, r := range c.Ranges {
		pairs = append(pairs, [2]int{r.Start, r.End})
	}
	encoded, err := json.Marshal(pairs)
	if err != nil {
		slog.Error("Error encoding read ranges", "error", err)
		return ""
	}
	return string(encoded)
}

// decodeRanges is encodeRanges' inverse. Anything unparseable is treated
// as a full read: the column is an optimization over "the file was read",
// and failing open matches the behavior before ranges were tracked.
func decodeRanges(encoded string) Coverage {
	if encoded == "" {
		return FullCoverage
	}
	var pairs [][2]int
	if err := json.Unmarshal([]byte(encoded), &pairs); err != nil {
		slog.Error("Error decoding read ranges", "error", err, "value", encoded)
		return FullCoverage
	}
	ranges := make([]LineRange, 0, len(pairs))
	for _, p := range pairs {
		ranges = append(ranges, LineRange{Start: p[0], End: p[1]})
	}
	return Coverage{Ranges: ranges}
}
