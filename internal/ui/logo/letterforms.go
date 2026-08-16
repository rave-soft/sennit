package logo

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/MakeNowJust/heredoc"
	"github.com/charmbracelet/x/exp/slice"
)

// renderWord renders letterforms to fork a word. stretchIndex is the index of
// the letter to stretch, or -1 if no letter should be stretched.
func renderWord(spacing int, stretchIndex int, letterforms ...letterform) string {
	if spacing < 0 {
		spacing = 0
	}

	renderedLetterforms := make([]string, len(letterforms))

	// pick one letter randomly to stretch
	for i, letter := range letterforms {
		renderedLetterforms[i] = letter(i == stretchIndex)
	}

	if spacing > 0 {
		// Add spaces between the letters and render.
		renderedLetterforms = slice.Intersperse(renderedLetterforms, strings.Repeat(" ", spacing))
	}
	return strings.TrimSpace(
		lipgloss.JoinHorizontal(lipgloss.Top, renderedLetterforms...),
	)
}

func joinLetterform(letters ...string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, letters...)
}

// letterformProps defines letterform stretching properties.
// for readability.
type letterformProps struct {
	width      int
	minStretch int
	maxStretch int
	stretch    bool
}

// stretchLetterformPart is a helper function for letter stretching. If randomize
// is false the minimum number will be used.
func stretchLetterformPart(s string, p letterformProps) string {
	if p.maxStretch < p.minStretch {
		p.minStretch, p.maxStretch = p.maxStretch, p.minStretch
	}
	n := p.width
	if p.stretch {
		n = cachedRandN(p.maxStretch-p.minStretch) + p.minStretch //nolint:gosec
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = s
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// LetterI renders the letter I in a stylized way. The stretch argument is
// accepted for symmetry with the other letterforms but ignored: a single
// vertical stroke has nothing to stretch.
func LetterI(_ bool) string {
	// Here's what we're making:
	//
	// █
	// █
	// ▀

	return heredoc.Doc(`
		█
		█
		▀`)
}

// LetterE renders the letter E in a stylized way. It takes a bool that
// determines whether to stretch the letter.
func LetterE(stretch bool) string {
	// Here's what we're making. There's no right column at all: three bars
	// hang off a single full-height stem, unlike B/D/R which close the
	// shape with a wall on the right.
	//
	// █▀▀
	// █▀▀
	// ▀▀▀

	left := heredoc.Doc(`
		█
		█
		▀`)
	middle := heredoc.Doc(`
		▀
		▀
		▀`)
	return joinLetterform(
		left,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      2,
			minStretch: 7,
			maxStretch: 12,
		}),
	)
}

// LetterN renders the letter N in a stylized way. It takes a bool that
// determines whether to stretch the letter.
func LetterN(stretch bool) string {
	// Here's what we're making. The top row is closed, joining the two
	// full-height walls, while the bottom row is left open in the middle —
	// that's what separates N from D (closed bottom) and A (filled middle
	// row).
	//
	// █▀▀▄
	// █  █
	// ▀  ▀
	//
	// There's no diagonal stroke here, and there can't be: middle is
	// repeated wholesale under stretch, so any diagonal cell would just
	// smear into another solid bar as the letter stretches, indistinguishable
	// from a straight crossbar. That's a hard limit of this system, not an
	// oversight — no letterform in this font carries a true diagonal, which
	// keeps N airy instead of gaining the mass a crossbar (as tried and
	// rejected: `█▄▄█` on row 2) would add, making it read too close to A.

	left := heredoc.Doc(`
		█
		█
		▀`)
	middle := heredoc.Doc(`
		▀

	`)
	right := heredoc.Doc(`
		▄
		█
		▀`)
	return joinLetterform(
		left,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      2,
			minStretch: 7,
			maxStretch: 12,
		}),
		right,
	)
}

// LetterS renders the letter S in a stylized way. It takes a bool that
// determines whether to stretch the letter.
func LetterS(stretch bool) string {
	// Here's what we're making. The full-height █ at the top-left and the
	// blank at bottom-left are the trick: row 2's left cell is deliberately
	// empty, because an S has no left wall in its lower half — that's what
	// separates it from a stack of bars, where every row would carry the
	// same left edge. The bottom-right █ mirrors it, closing the curve on
	// the other side. Together the two diagonally-opposite corners trace
	// the stroke.
	//
	// █▀▀▀
	//  ▀▀█
	// ▀▀▀▀

	left := heredoc.Doc(`
		█

		▀`)
	middle := heredoc.Doc(`
		▀
		▀
		▀`)
	right := heredoc.Doc(`
		▀
		█
		▀`)
	return joinLetterform(
		left,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      2,
			minStretch: 7,
			maxStretch: 12,
		}),
		right,
	)
}

// LetterT renders the letter T in a stylized way. The stretch argument is
// accepted for symmetry with the other letterforms but ignored: T's stem
// has to stay centred under the bar, and stretching only one side would
// push it off-centre.
func LetterT(_ bool) string {
	// Here's what we're making:
	//
	// ▀▀█▀▀
	//   █
	//   ▀

	bar := heredoc.Doc(`
		▀

	`)
	stem := heredoc.Doc(`
		█
		█
		▀`)
	barPart := stretchLetterformPart(bar, letterformProps{
		stretch: false,
		width:   2,
	})
	return joinLetterform(barPart, stem, barPart)
}
