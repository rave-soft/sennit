package attachments

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newTestRenderer() *Renderer {
	sty := styles.SennitDark()
	return NewRenderer(
		sty.Attachments.Normal,
		sty.Attachments.Deleting,
		sty.Attachments.Image,
		sty.Attachments.Text,
		sty.Attachments.Skill,
		sty.Attachments.Remove,
	)
}

func TestRender_IncludesRemoveButton(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "test.txt"},
	}
	out := r.Render(atts, false, true, 80)
	require.Contains(t, out, styles.RemoveIcon)
}

func TestSetHoverHighlightsRemoveButton(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	r := NewRenderer(
		sty.Attachments.Normal,
		sty.Attachments.Deleting,
		sty.Attachments.Image,
		sty.Attachments.Text,
		sty.Attachments.Skill,
		sty.Attachments.Remove,
		sty.Attachments.RemoveHover,
	)
	m := New(r, Keymap{})
	m.list = []message.Attachment{{FileName: "test.txt"}}
	_ = m.Render(80)
	x := r.bounds[0].startX

	m.SetHover(x)
	require.Equal(t, 0, r.hoveredRemove)
	m.SetHover(-1)
	require.Equal(t, -1, r.hoveredRemove)
}

func TestRender_DeletingModeNoRemoveButton(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "test.txt"},
	}
	out := r.Render(atts, true, true, 80)
	require.NotContains(t, out, styles.RemoveIcon)
}

func TestRender_ShowRemoveFalseOmitsRemoveButton(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "no-change.png"},
	}
	out := r.Render(atts, false, false, 80)
	require.NotContains(t, out, styles.RemoveIcon,
		"posted-message attachments must not show a remove button")
	require.Empty(t, r.bounds,
		"no remove bounds should be recorded when the button is hidden")
	require.Equal(t, -1, r.HitTestRemove(atts, 0))
}

func TestRender_ShowRemoveFalseKeepsGapBetweenChips(t *testing.T) {
	t.Parallel()

	// Regression for the #134 + #135 interaction: #134 moved the trailing
	// margin onto the remove button, and #135 hides that button on posted
	// messages. Together, posted messages with multiple attachments lost the
	// margin that separated adjacent chips, so their backgrounds touched. The
	// filename must carry the margin when the remove button is hidden.
	//
	// White-box width check: the visible width of the two chips without any
	// separator is icon+filename per chip. With the fix each posted chip adds
	// a 1-column trailing margin, so the rendered row is exactly two columns
	// wider. Stripping ANSI can't detect this (a margin space and a
	// background-colored padding space are both just spaces), so we measure
	// width instead.
	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "alpha.txt"},
		{FileName: "beta.txt"},
	}
	bare := lipgloss.Width(r.textStyle.String()+r.normalStyle.Render("alpha.txt")) +
		lipgloss.Width(r.textStyle.String()+r.normalStyle.Render("beta.txt"))

	got := lipgloss.Width(r.Render(atts, false, false, 200))
	require.Equal(t, bare+2, got,
		"each posted chip must carry a 1-col trailing margin so adjacent chip backgrounds don't touch")
}

func TestRender_DeletingModeKeepsChipLayout(t *testing.T) {
	t.Parallel()

	// Regression for review feedback on #3338: entering delete-mode used
	// to replace the leading icon with the numeral and drop the remove
	// button, shifting every chip. The numeral must instead take over the
	// remove button's slot, leaving the left side of the chip as-is.
	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "main.go"},
		{FileName: "models.go"},
	}
	idle := r.Render(atts, false, true, 200)
	deleting := r.Render(atts, true, true, 200)

	require.Equal(t, lipgloss.Width(idle), lipgloss.Width(deleting),
		"entering delete-mode must not shift the chips")
	require.Contains(t, deleting, styles.TextIcon,
		"delete-mode must keep the chip's icon")
	require.Contains(t, deleting, "0")
	require.Contains(t, deleting, "1")
}

func TestRender_RemoveButtonHasRightPadding(t *testing.T) {
	t.Parallel()

	// Regression for review feedback on #3338: the ✕ must not sit flush
	// against the right edge of its colored box. The cell to the right of the
	// glyph has to be padding — part of the button's background — rather than
	// a transparent margin, so the glyph has breathing room on its right.
	//
	// A plain-width or ANSI-stripped check can't catch this: a margin space
	// and a background-colored padding space are both one blank column. So we
	// inspect the per-cell background and assert the button's background
	// extends one cell past the ✕.
	r := newTestRenderer()
	atts := []message.Attachment{{FileName: "main.go"}}
	out := r.Render(atts, false, true, 200)

	cells := parseCells(out)
	xi := -1
	for i, c := range cells {
		if c.r == styles.RemoveIcon {
			xi = i
			break
		}
	}
	require.GreaterOrEqual(t, xi, 0, "rendered output must contain the ✕ glyph")
	require.NotEmpty(t, cells[xi].bg, "the ✕ cell must have the button's background")
	require.Less(t, xi+1, len(cells),
		"the ✕ must be followed by a trailing padding cell, not be the box's last cell")
	require.Equal(t, cells[xi].bg, cells[xi+1].bg,
		"the cell to the right of ✕ must share the button's background (padding), not be a transparent margin")
}

func TestRender_RemoveButtonKeepsGapBetweenChips(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.txt"},
	}
	cells := parseCells(r.Render(atts, false, true, 200))

	xi := -1
	for i, c := range cells {
		if c.r == styles.RemoveIcon {
			xi = i
			break
		}
	}
	require.GreaterOrEqual(t, xi, 0)
	require.Less(t, xi+2, len(cells))
	require.Empty(t, cells[xi+2].bg, "adjacent attachment chips must have a transparent one-cell gap")
}

// cell is one rendered terminal cell: its rune and the truecolor background
// in effect ("r;g;b", or "" for none).
type cell struct {
	r  string
	bg string
}

// parseCells walks a lipgloss-rendered string and returns its visible cells
// with the background color active at each. It understands the SGR sequences
// lipgloss emits (truecolor 48;2;r;g;b backgrounds, 38;2;r;g;b foregrounds,
// and resets); other escapes are ignored.
func parseCells(s string) []cell {
	var cells []cell
	bg := ""
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				bg = applyBG(s[i+2:j], bg)
				i = j + 1
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		cells = append(cells, cell{r: s[i : i+size], bg: bg})
		i += size
	}
	return cells
}

// applyBG updates the current background given one SGR parameter string.
func applyBG(params, cur string) string {
	if params == "" || params == "0" {
		return ""
	}
	toks := strings.Split(params, ";")
	for k := 0; k < len(toks); k++ {
		switch toks[k] {
		case "0":
			cur = ""
		case "38": // foreground — skip its arguments
			if k+1 < len(toks) && toks[k+1] == "2" {
				k += 4
			} else if k+1 < len(toks) && toks[k+1] == "5" {
				k += 2
			}
		case "48": // background
			if k+4 < len(toks) && toks[k+1] == "2" {
				cur = toks[k+2] + ";" + toks[k+3] + ";" + toks[k+4]
				k += 4
			} else if k+2 < len(toks) && toks[k+1] == "5" {
				cur = toks[k+2]
				k += 2
			}
		}
	}
	return cur
}

func TestRender_MultipleChipsEachHaveRemoveButton(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "a.txt"},
		{FileName: "b.txt"},
		{FileName: "c.txt"},
	}
	out := r.Render(atts, false, true, 120)
	// Count occurrences of the remove glyph.
	count := 0
	for _, c := range out {
		if string(c) == styles.RemoveIcon {
			count++
		}
	}
	require.Equal(t, 3, count, "each chip should have a remove button")
}

func TestHitTestRemove_ClickOnFirstChipRemove(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.txt"},
	}
	_ = r.Render(atts, false, true, 120)

	// The remove button of the first chip should be hit-testable.
	// Click at various X positions to verify we hit the right chip.
	idx := r.HitTestRemove(atts, 0)
	// At x=0 we're on the icon, not the remove button.
	require.Equal(t, -1, idx)
}

func TestHitTestRemove_ReturnsCorrectIndex(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.txt"},
	}
	_ = r.Render(atts, false, true, 120)

	// Each chip bounds are stored after render. Verify there are two.
	require.Len(t, r.bounds, 2)

	// Click on the first chip's remove button.
	b0 := r.bounds[0]
	idx := r.HitTestRemove(atts, b0.startX)
	require.Equal(t, 0, idx)

	// Click on the second chip's remove button.
	b1 := r.bounds[1]
	idx = r.HitTestRemove(atts, b1.startX)
	require.Equal(t, 1, idx)
}

func TestHitTestRemove_TrailingMarginNotClickable(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.txt"},
	}
	_ = r.Render(atts, false, true, 120)

	// The cell just past a button's hit region belongs to the next chip, not
	// to this button — a click there must not remove this attachment.
	b0 := r.bounds[0]
	require.Equal(t, -1, r.HitTestRemove(atts, b0.removeEnd))
}

func TestHitTestRemove_OutsideAnyRemoveReturnsMinusOne(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "test.txt"},
	}
	_ = r.Render(atts, false, true, 80)

	// Click far past the remove button.
	idx := r.HitTestRemove(atts, 999)
	require.Equal(t, -1, idx)
}

func TestHandleClick_RemovesAttachment(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	km := Keymap{}
	m := New(r, km)
	m.list = []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.txt"},
	}

	// Render so bounds are populated.
	_ = m.Render(120)

	// Click the first chip's remove button.
	b0 := r.bounds[0]
	handled := m.HandleClick(b0.startX)
	require.True(t, handled)
	require.Len(t, m.list, 1)
	require.Equal(t, "second.txt", m.list[0].FileName)
}

func TestHandleClick_ClickOutsideRemoveDoesNothing(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	km := Keymap{}
	m := New(r, km)
	m.list = []message.Attachment{
		{FileName: "test.txt"},
	}

	_ = m.Render(80)

	// Click at x=0 (the icon area, not the remove button).
	handled := m.HandleClick(0)
	require.False(t, handled)
	require.Len(t, m.list, 1)
}

func TestHandleClick_DeletingModeIgnored(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	km := Keymap{}
	m := New(r, km)
	m.list = []message.Attachment{
		{FileName: "test.txt"},
	}
	m.deleting = true

	_ = m.Render(80)

	// bounds are empty in deleting mode since remove buttons aren't rendered.
	require.Empty(t, r.bounds)
	// Click anywhere — should be ignored.
	handled := m.HandleClick(10)
	require.False(t, handled)
}

func TestHandleClick_EmptyListIgnored(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	km := Keymap{}
	m := New(r, km)

	handled := m.HandleClick(5)
	require.False(t, handled)
}

// TestRender_NoMoreChipWhenEverythingFits is a regression test for an
// off-by-one in the "N more…" summary chip: the loop appended it as
// soon as i == fits, guarded only by len(attachments) > i — which is
// true even at i == len(attachments)-1, the last attachment, already
// fully rendered on its own. That showed a bogus "1 more…" chip when
// nothing was actually hidden. Sizing the width so fits lands exactly
// on the last index must not produce a summary chip.
func TestRender_NoMoreChipWhenEverythingFits(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "a.txt"},
		{FileName: "b.txt"},
		{FileName: "c.txt"},
	}

	// Mirrors Render's own maxItemWidth formula so a width can be chosen
	// that drives `fits` to exactly len(atts)-1 — the boundary where the
	// bug fired.
	removeReserve := r.removeStyle.String()
	maxItemWidth := lipgloss.Width(r.imageStyle.String() + r.normalStyle.Render(strings.Repeat("x", maxFilename)) + removeReserve)
	width := maxItemWidth * len(atts)

	out := r.Render(atts, false, true, width)
	require.NotContains(t, out, "more…", "every attachment fit; no summary chip should appear")
	for _, a := range atts {
		require.Contains(t, out, a.FileName)
	}
}

var moreOrCountRE = regexp.MustCompile(`(\d+) (?:more|attachments)…`)

// TestRender_ChipsPlusHiddenCountEqualsTotal is a regression test for two
// bugs in the overflow cap: (a) the "N more…" summary counted the chip at
// the cutoff index as still hidden, overstating the true hidden count by
// one; (b) when the row was narrower than a single chip, `fits` went
// negative and the cap never triggered at all, so every chip rendered and
// overflowed the row instead of collapsing.
//
// The invariant under test: at every width, the number of chips actually
// drawn (one ✕ per chip) plus whatever count the summary reports as
// hidden must equal the total number of attachments — never more, never
// less. That invariant alone doesn't catch (b): drawing every chip with
// no summary at all also happens to sum correctly (3 drawn + 0 hidden =
// 3), even though the row overflows its width. So each case also pins
// down exactly how many chips a correct implementation draws, which
// fails on "exactly one chip's worth" (summary overstated the hidden
// count by one) and "narrower than one chip" (nothing capped, so all
// three chips drew) before the fix.
func TestRender_ChipsPlusHiddenCountEqualsTotal(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "a.txt"},
		{FileName: "b.txt"},
		{FileName: "c.txt"},
	}

	removeReserve := r.removeStyle.String()
	maxItemWidth := lipgloss.Width(r.imageStyle.String() + r.normalStyle.Render(strings.Repeat("x", maxFilename)) + removeReserve)

	tests := []struct {
		name       string
		width      int
		wantShown  int
		wantHidden int
	}{
		{"comfortably wide", maxItemWidth * (len(atts) + 2), 3, 0},
		{"exactly one chip's worth", maxItemWidth, 1, 2},
		{"narrower than one chip", maxItemWidth - 1, 0, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Render mutates the renderer's hit-test bounds, so each
			// parallel subtest needs its own instance rather than
			// sharing r.
			out := newTestRenderer().Render(atts, false, true, tt.width)

			shown := strings.Count(out, styles.RemoveIcon)

			hidden := 0
			if m := moreOrCountRE.FindStringSubmatch(out); m != nil {
				n, err := strconv.Atoi(m[1])
				require.NoError(t, err)
				hidden = n
			}

			require.Equal(t, len(atts), shown+hidden,
				"chips drawn (%d) plus hidden count (%d) must equal total attachments (%d)",
				shown, hidden, len(atts))
			require.Equal(t, tt.wantShown, shown, "unexpected number of chips drawn")
			require.Equal(t, tt.wantHidden, hidden, "unexpected hidden count reported")
		})
	}
}

// TestUpdate_DeleteMode_TwoDigitIndex is a regression test for delete
// mode only ever consuming a single digit: with more than 10
// attachments, the first keystroke used to commit (and exit delete
// mode) immediately, making any index past 9 unreachable. A second
// digit must extend the buffered index instead of being dropped.
func TestUpdate_DeleteMode_TwoDigitIndex(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	m := New(r, Keymap{})
	for i := range 15 {
		m.list = append(m.list, message.Attachment{FileName: fmt.Sprintf("file%d.txt", i)})
	}
	m.deleting = true

	// First digit ('1'): with 15 attachments (max index 14, two digits),
	// this must NOT commit yet — it could still be extended into "12".
	handled := m.Update(tea.KeyPressMsg{Code: '1'})
	require.True(t, handled)
	require.True(t, m.deleting, "a single digit must not exit delete mode when a second digit could still refine the index")
	require.Len(t, m.list, 15, "nothing should be deleted until the index is unambiguous")

	// Second digit ('2') completes "12".
	handled = m.Update(tea.KeyPressMsg{Code: '2'})
	require.True(t, handled)
	require.False(t, m.deleting, "delete mode must exit once the index is complete")
	require.Len(t, m.list, 14)
	for _, a := range m.list {
		require.NotEqual(t, "file12.txt", a.FileName)
	}
}

// TestUpdate_DeleteMode_SingleDigitStillCommitsImmediately covers the
// common case (10 or fewer attachments): every valid index is a single
// digit, so there is nothing a second digit could refine, and the
// original one-keystroke behavior must be preserved.
func TestUpdate_DeleteMode_SingleDigitStillCommitsImmediately(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	m := New(r, Keymap{})
	for i := range 3 {
		m.list = append(m.list, message.Attachment{FileName: fmt.Sprintf("file%d.txt", i)})
	}
	m.deleting = true

	handled := m.Update(tea.KeyPressMsg{Code: '1'})
	require.True(t, handled)
	require.False(t, m.deleting, "a single digit must commit immediately when no further digit could be valid")
	require.Len(t, m.list, 2)
	require.Equal(t, "file0.txt", m.list[0].FileName)
	require.Equal(t, "file2.txt", m.list[1].FileName)
}
