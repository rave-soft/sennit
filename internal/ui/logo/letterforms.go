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

// LetterR renders the letter R in a stylized way. It takes an integer that
// determines how many cells to stretch the letter. If the stretch is less than
// 1, it defaults to no stretching.
func LetterR(stretch bool) string {
	// Here's what we're making:
	//
	// █▀▀▀▄
	// █▀▀▀▄
	// ▀   ▀

	left := heredoc.Doc(`
		█
		█
		▀
	`)
	center := heredoc.Doc(`
		▀
		▀
	`)
	right := heredoc.Doc(`
		▄
		▄
		▀
	`)
	return joinLetterform(
		left,
		stretchLetterformPart(center, letterformProps{
			stretch:    stretch,
			width:      3,
			minStretch: 7,
			maxStretch: 12,
		}),
		right,
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

// LetterB renders the letter B in a stylized way. It takes a bool that
// determines whether to stretch the letter.
func LetterB(stretch bool) string {
	// Here's what we're making. The middle row is a solid crossbar (█
	// instead of ▀), giving B an actual waist to anchor on instead of
	// implying one through half-block shading — that's what sets it apart
	// from R, which leaves that row open.
	//
	// █▀▀▄
	// ███▄
	// ▀▀▀▀

	left := heredoc.Doc(`
		█
		█
		▀`)
	middle := heredoc.Doc(`
		▀
		█
		▀`)
	right := heredoc.Doc(`
		▄
		▄
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

// LetterA renders the letter A in a stylized way. The stretch argument is
// accepted for symmetry with the other letterforms but ignored: A's
// tent-like silhouette stops reading as a letter at any elongation (like
// LetterI, which has nothing to stretch).
func LetterA(_ bool) string {
	// Here's what we're making:
	//
	// ▄▀▀▀▄
	// █▀▀▀█
	// ▀   ▀

	left := heredoc.Doc(`
		▄
		█
		▀`)
	middle := heredoc.Doc(`
		▀
		▀
	`)
	right := heredoc.Doc(`
		▄
		█
		▀`)
	return joinLetterform(
		left,
		stretchLetterformPart(middle, letterformProps{
			stretch: false,
			width:   3,
		}),
		right,
	)
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

// LetterD renders the letter D in a stylized way. It takes a bool that
// determines whether to stretch the letter.
func LetterD(stretch bool) string {
	// Here's what we're making. The bottom row is pure ▀ (plus padding),
	// same as every other letterform in this word — a closing curve made
	// of █/▄ would sit below the shared baseline and make D look taller
	// than its neighbors. Rounding comes only from the ▄ cap on top; the
	// wall itself (▄, █, ▀) stays closed on every row.
	//
	// █▀▀▄
	// █  █
	// ▀▀▀▀

	left := heredoc.Doc(`
		█
		█
		▀`)
	middle := heredoc.Doc(`
		▀

		▀`)
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
