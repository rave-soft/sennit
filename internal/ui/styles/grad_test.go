package styles

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

// TestForegroundGrad_BoldSetsSGR is the regression test for grad.go
// discarding style.Bold(true)'s result: lipgloss styles are immutable, so
// the bolded style must be reassigned rather than dropped. Before the fix,
// bold=true and bold=false rendered identical ANSI, and the wordmark was
// never actually bold.
func TestForegroundGrad_BoldSetsSGR(t *testing.T) {
	t.Parallel()

	base := lipgloss.NewStyle()
	color1 := color.RGBA{R: 255, A: 255}
	color2 := color.RGBA{B: 255, A: 255}

	// Single-rune input exercises the len(input) == 1 shortcut branch;
	// multi-rune input exercises the gradient-ramp loop. Both had the same
	// discarded-Bold bug.
	for name, input := range map[string]string{"single-rune": "x", "multi-rune": "hello"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			plain := strings.Join(ForegroundGrad(base, input, false, color1, color2), "")
			bold := strings.Join(ForegroundGrad(base, input, true, color1, color2), "")

			require.NotEqual(t, plain, bold)
			require.Contains(t, bold, "\x1b[1;", "bold output must carry the SGR bold code")
			require.NotContains(t, plain, "\x1b[1;", "non-bold output must not carry the SGR bold code")
		})
	}
}
