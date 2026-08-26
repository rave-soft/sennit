package spin

import (
	"image/color"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

func demoSettings(id string, mode Mode) Settings {
	return Settings{
		ID:          id,
		Size:        15,
		Label:       "Thinking",
		Mode:        mode,
		GradColorA:  color.RGBA{R: 0x7a, G: 0xc7, B: 0xc4, A: 0xff},
		GradColorB:  color.RGBA{R: 0x2f, G: 0x6f, B: 0x8f, A: 0xff},
		LabelColor:  color.RGBA{R: 0x70, G: 0x70, B: 0x70, A: 0xff},
		CycleColors: true,
	}
}

// frames renders n consecutive frames with every SGR sequence stripped —
// what a terminal with no colour actually shows.
func frames(a *Anim, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = sgr.ReplaceAllString(a.Render(), "")
		a.Animate(StepMsg{ID: a.id})
	}
	return out
}

// TestPulseMovesWithoutColor is the regression for the mode's first
// implementation, which moved a colour along a row of identical dots.
// That looks right on a truecolor terminal and is a motionless row
// everywhere colorprofile degrades to Ascii — under NO_COLOR, or on a
// dumb terminal — which is precisely where someone who dislikes the
// scramble is most likely to be. The motion has to live in the glyphs.
func TestPulseMovesWithoutColor(t *testing.T) {
	t.Parallel()

	got := frames(New(demoSettings("pulse-mono", ModePulse)), 4)
	for i := 1; i < len(got); i++ {
		require.NotEqual(t, got[i-1], got[i],
			"consecutive pulse frames are identical once colour is stripped")
	}
}

// TestPulseSweepIsSeamless pins that the band's period is its width: the
// frame after the last is the sweep continuing, not the comet jumping
// back. Rendering one full cycle and one more frame must return to the
// starting frame exactly.
func TestPulseSweepIsSeamless(t *testing.T) {
	t.Parallel()

	a := New(demoSettings("pulse-wrap", ModePulse))
	period := len(a.cyclingFrames)
	got := frames(a, period+1)

	// The band only. The label's ellipsis runs on its own, longer
	// period, so comparing whole renders would be asking the wrong
	// question and would fail on a frame where the band had in fact
	// closed the loop.
	band := func(frame string) string { return string([]rune(frame)[:a.cyclingCharWidth]) }

	require.Equal(t, band(got[0]), band(got[period]),
		"the sweep must close on itself after one period")
}

// TestDotsIsOneGlyph pins the point of the mode: a single spinner
// character, whatever Size asks for, and one that changes over time.
func TestDotsIsOneGlyph(t *testing.T) {
	t.Parallel()

	a := New(demoSettings("dots", ModeDots))
	require.Equal(t, 1, a.cyclingCharWidth, "Size must not widen the dots spinner")

	got := frames(a, len(brailleFrames)*dotsFrameHold)
	seen := map[string]bool{}
	for _, f := range got {
		seen[strings.SplitN(f, " ", 2)[0]] = true
	}
	require.Len(t, seen, len(brailleFrames), "every braille frame must be reachable")
}

// TestNoneDrawsNoBand pins that ModeNone leaves the label and nothing
// else: no band, and so nothing whose width the label has to sit past.
func TestNoneDrawsNoBand(t *testing.T) {
	t.Parallel()

	a := New(demoSettings("none", ModeNone))
	require.Equal(t, 0, a.cyclingCharWidth)

	for _, f := range frames(a, 6) {
		require.True(t, strings.HasPrefix(f, "Thinking"),
			"ModeNone rendered something before the label: %q", f)
	}
}

// TestScrambleIsUnchanged guards the default: adding modes must not have
// altered what a call site that says nothing about motion renders.
func TestScrambleIsUnchanged(t *testing.T) {
	t.Parallel()

	unset := demoSettings("same", ModeScramble)
	unset.Mode = ""
	require.Equal(t, frames(New(demoSettings("same", ModeScramble)), 8), frames(New(unset), 8))
}
