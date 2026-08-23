package common

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestUnderlineColumnRange_AccountsForWideRunes is a regression test for
// button.go's underline placement: it used to feed the character index
// straight into a cell-width range, which is only correct if every
// preceding rune is exactly one cell wide. A wide rune (e.g. a
// double-width CJK character) before the target shifts its column by
// more than one per rune, so a byte/rune-index shortcut lands the
// underline one cell short.
func TestUnderlineColumnRange_AccountsForWideRunes(t *testing.T) {
	t.Parallel()

	// "日" is double-width, so "X" (rune index 1) sits at column 2, not
	// column 1.
	start, end := underlineColumnRange("日X", 1)
	require.Equal(t, 2, start)
	require.Equal(t, 3, end)
}

// TestUnderlineColumnRange_MultiByteNarrowRune covers the common case of
// a multi-byte but single-width rune (e.g. accented Latin) ahead of the
// target: the column must still track the rune count, not the byte
// count.
func TestUnderlineColumnRange_MultiByteNarrowRune(t *testing.T) {
	t.Parallel()

	// "é" is 2 bytes but 1 cell wide, so "r" (rune index 3 in "café")
	// sits at column 3.
	start, end := underlineColumnRange("café", 3)
	require.Equal(t, 3, start)
	require.Equal(t, 4, end)
}

// TestButton_UnderlineIndexBoundCheckUsesRuneCount is a regression test
// for the out-of-range guard in Button: it used to compare UnderlineIndex
// against len(text)-1 (a byte count), which is larger than the number of
// characters for any multi-byte label. That let an actually-out-of-range
// character index slip past the guard instead of being reset to -1.
func TestButton_UnderlineIndexBoundCheckUsesRuneCount(t *testing.T) {
	t.Parallel()

	var sty styles.Styles
	sty.Button.Blurred = lipgloss.NewStyle()

	// "é" is a single character encoded in 2 bytes. Index 1 is not a
	// valid character index (there is only index 0), but len("é")-1 == 1
	// would have passed the old byte-length bound check.
	withOutOfRangeIndex := Button(&sty, ButtonOpts{Text: "é", UnderlineIndex: 1})
	withNoUnderline := Button(&sty, ButtonOpts{Text: "é", UnderlineIndex: -1})

	require.Equal(t, withNoUnderline, withOutOfRangeIndex,
		"an out-of-range character index must be treated the same as no underline")
}
