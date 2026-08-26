package accounts

import (
	"fmt"
	"regexp"
	"strings"
)

// idPrefix marks every account ID this package generates, so an ID found
// in the wild is recognizably ours regardless of provider.
const idPrefix = "acc_"

// maxNormalizedIDLen bounds how much of a provider's account identifier
// survives normalization. Provider account IDs (JWT subjects, opaque
// tokens) can be long; a full one would make IDs unwieldy to read or type
// without adding anything an ID needs — uniqueness is guaranteed by the
// collision suffix below, not by keeping every character.
const maxNormalizedIDLen = 40

// repeatedDashes collapses runs of '-' left behind by normalization, e.g.
// from "foo@@bar" becoming "foo--bar".
var repeatedDashes = regexp.MustCompile(`-+`)

// NextID returns an ID for a new account, unique among existing.
//
// When accountID is non-empty, the ID is derived from it (lowercased,
// non [a-z0-9_-] characters replaced with '-', repeats collapsed,
// truncated), so the ID stays recognizable to whoever's debugging. When
// accountID is empty — the case for API-key providers, which have
// nothing provider-side to derive an ID from — NextID assigns the
// smallest unused "acc_N".
//
// Either way, if the resulting ID collides with one already in existing,
// a numeric suffix ("-2", "-3", ...) is appended until an unused ID is
// found. The result is a pure function of the inputs: no randomness, no
// clock, so the same (existing, accountID) always yields the same ID.
func NextID(existing []Account, accountID string) string {
	used := make(map[string]bool, len(existing))
	for _, a := range existing {
		used[a.ID] = true
	}

	base := normalizedIDFor(accountID)
	if base == "" {
		base = smallestFreeNumericID(used)
	}
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !used[candidate] {
			return candidate
		}
	}
}

// normalizedIDFor builds the "acc_"-prefixed ID for a non-empty
// accountID, or "" if accountID is empty or normalizes to nothing (e.g.
// it was made entirely of characters normalization strips).
func normalizedIDFor(accountID string) string {
	if accountID == "" {
		return ""
	}
	norm := normalizeAccountID(accountID)
	if norm == "" {
		return ""
	}
	return idPrefix + norm
}

// normalizeAccountID turns a provider account identifier into the
// lowercase, dash-separated form usable inside an ID.
func normalizeAccountID(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := repeatedDashes.ReplaceAllString(b.String(), "-")
	out = strings.Trim(out, "-")
	if len(out) > maxNormalizedIDLen {
		out = strings.TrimRight(out[:maxNormalizedIDLen], "-")
	}
	return out
}

// smallestFreeNumericID returns the lowest-numbered "acc_N" not present
// in used, starting at 1. This is what fills a gap: if acc_1 and acc_3
// exist but acc_2 was removed, the next account gets acc_2 back rather
// than acc_4.
func smallestFreeNumericID(used map[string]bool) string {
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s%d", idPrefix, i)
		if !used[candidate] {
			return candidate
		}
	}
}
