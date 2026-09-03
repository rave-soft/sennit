package filetracker

import (
	"encoding/json"
	"log/slog"

	"github.com/rave-soft/sennit/internal/filetracker/coverage"
)

// Coverage and LineRange are aliases onto the leaf coverage package so
// callers here keep their existing names. The interval arithmetic itself
// (Covers, Empty, Add, Shift) lives there — see that package's doc comment
// for why it had to move out of filetracker: internal/agent/tools needs
// the same logic but must not import filetracker, which pulls in
// internal/db.
type Coverage = coverage.Coverage

type LineRange = coverage.LineRange

// FullCoverage is the coverage of a file that was read end to end.
var FullCoverage = coverage.FullCoverage

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

// decodeCoverage decodes the stored ranges for a read-modify-write that
// knows whether a row exists yet. A row holding "" means the file was
// read end to end; no row at all means nothing has been read, which is
// empty coverage, not full — encodeRanges/decodeRanges alone cannot tell
// these apart because both encode to "".
func decodeCoverage(encoded string, exists bool) Coverage {
	if !exists {
		return Coverage{}
	}
	return decodeRanges(encoded)
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
