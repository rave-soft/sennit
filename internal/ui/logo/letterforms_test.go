package logo

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

// TestLetterforms verifies the house style shared by every letterform: each
// one is exactly 3 rows tall, every row has the same display width, and
// nothing renders below the shared baseline (the bottom row is only ▀ or
// space).
func TestLetterforms(t *testing.T) {
	t.Parallel()

	letterforms := map[string]letterform{
		"B": LetterB,
		"R": LetterR,
		"A": LetterA,
		"I": LetterI,
		"D": LetterD,
		"E": LetterE,
		"N": LetterN,
		"S": LetterS,
		"T": LetterT,
	}

	for name, fn := range letterforms {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, stretch := range []bool{false, true} {
				rendered := fn(stretch)
				lines := strings.Split(rendered, "\n")
				require.Lenf(t, lines, 3, "letterform %s (stretch=%v) must render exactly 3 lines, got %q", name, stretch, rendered)

				width := lipgloss.Width(lines[0])
				for i, line := range lines {
					require.Equalf(t, width, lipgloss.Width(line), "letterform %s (stretch=%v) line %d width mismatch: %q", name, stretch, i, rendered)
				}

				baseline := lines[2]
				for _, r := range baseline {
					require.Containsf(t, "▀ ", string(r), "letterform %s (stretch=%v) baseline row must contain only ▀ or space, got %q", name, stretch, baseline)
				}
			}
		})
	}
}
