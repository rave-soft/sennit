package common

import (
	"image/color"
	"testing"

	"github.com/stretchr/testify/require"
)

func testPalette() [16]color.Color {
	var p [16]color.Color
	for i := range p {
		// Distinct, easy-to-assert colors: R=index, G=0, B=0.
		p[i] = color.RGBA{R: uint8(i), G: 0, B: 0, A: 0xFF}
	}
	return p
}

func TestRemapANSI16(t *testing.T) {
	t.Parallel()

	pal := testPalette()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no escapes passes through",
			in:   "plain text",
			want: "plain text",
		},
		{
			name: "standard foreground red is remapped",
			in:   "\x1b[31mhi\x1b[0m",
			want: "\x1b[38;2;1;0;0mhi\x1b[0m",
		},
		{
			name: "bright foreground red is remapped",
			in:   "\x1b[91mhi\x1b[0m",
			want: "\x1b[38;2;9;0;0mhi\x1b[0m",
		},
		{
			name: "standard background green is remapped",
			in:   "\x1b[42mx\x1b[0m",
			want: "\x1b[48;2;2;0;0mx\x1b[0m",
		},
		{
			name: "bold plus color keeps bold, remaps color",
			in:   "\x1b[1;31mx\x1b[0m",
			want: "\x1b[1;38;2;1;0;0mx\x1b[0m",
		},
		{
			name: "256-color extended is left untouched",
			in:   "\x1b[38;5;196mx\x1b[0m",
			want: "\x1b[38;5;196mx\x1b[0m",
		},
		{
			name: "truecolor extended is left untouched",
			in:   "\x1b[38;2;10;20;30mx\x1b[0m",
			want: "\x1b[38;2;10;20;30mx\x1b[0m",
		},
		{
			name: "non-SGR CSI sequence untouched",
			in:   "\x1b[2J\x1b[31mx",
			want: "\x1b[2J\x1b[38;2;1;0;0mx",
		},
		{
			name: "reset and default fg left as-is",
			in:   "\x1b[0;39mx",
			want: "\x1b[0;39mx",
		},
		{
			name: "underline 256-color extended is left untouched",
			in:   "\x1b[58;5;196mx\x1b[0m",
			want: "\x1b[58;5;196mx\x1b[0m",
		},
		{
			name: "underline truecolor extended is left untouched",
			in:   "\x1b[58;2;10;20;30mx\x1b[0m",
			want: "\x1b[58;2;10;20;30mx\x1b[0m",
		},
		{
			name: "default underline color left as-is",
			in:   "\x1b[59mx",
			want: "\x1b[59mx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, RemapANSI16(tt.in, pal))
		})
	}
}

func TestStripCursorControl(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text passes through",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "SGR color sequences preserved",
			in:   "\x1b[31mred\x1b[0m",
			want: "\x1b[31mred\x1b[0m",
		},
		{
			name: "cursor up stripped",
			in:   "line1\n\x1b[Aline2",
			want: "line1\nline2",
		},
		{
			name: "cursor down stripped",
			in:   "top\x1b[Bbottom",
			want: "topbottom",
		},
		{
			name: "erase line stripped",
			in:   "progress\x1b[Kdone",
			want: "progressdone",
		},
		{
			name: "erase display stripped",
			in:   "\x1b[2Jcleared",
			want: "cleared",
		},
		{
			name: "cursor position stripped",
			in:   "\x1b[Hhome",
			want: "home",
		},
		{
			name: "DEC private mode hide cursor stripped",
			in:   "\x1b[?25linvisible\x1b[?25hvisible",
			want: "invisiblevisible",
		},
		{
			name: "DEC save restore cursor stripped",
			in:   "\x1b7saved\x1b8restored",
			want: "savedrestored",
		},
		{
			name: "save restore cursor CSI stripped",
			in:   "\x1b[spos\x1b[upos",
			want: "pospos",
		},
		{
			name: "scroll up down stripped",
			in:   "\x1b[Sscroll\x1b[Tscroll",
			want: "scrollscroll",
		},
		{
			// \r only returns the cursor to column 0; it doesn't erase
			// the rest of the line. "done!" (5 cells) overwrites the
			// first 5 cells of "loading..." (10 cells) — "load" — and
			// the remaining "ng..." stays visible past it, exactly as
			// it would on a real terminal.
			name: "carriage return simulates overwrite, preserving the uncovered tail",
			in:   "loading...\rdone!",
			want: "done!ng...",
		},
		{
			name: "multiple carriage returns keep last",
			in:   "step1\rstep2\rstep3",
			want: "step3",
		},
		{
			name: "carriage return per line",
			in:   "a\rb\nc\rd",
			want: "b\nd",
		},
		{
			// "done" (4 cells) overwrites the first 4 cells of "loading"
			// (7 cells, red) — "load" — leaving "ing" visible past it.
			// That surviving tail must keep the red SGR state it had
			// before the \r, not silently lose it.
			name: "mixed SGR and cursor control preserves color state across the overwrite",
			in:   "\x1b[31mloading\x1b[0m\r\x1b[32mdone\x1b[0m",
			want: "\x1b[32mdone\x1b[0m\x1b[31ming\x1b[0m",
		},
		{
			name: "git push style progress",
			in:   "Enumerating objects: 10\rEnumerating objects: 50\rEnumerating objects: 100, done.",
			want: "Enumerating objects: 100, done.",
		},
		{
			// The standard progress-bar clear-and-redraw pattern:
			// return to column 0, erase-in-line, then draw the
			// replacement. Real terminals show only "Done" — the erase
			// must drop the uuncovered tail that a bare \r (with no K)
			// would otherwise have preserved, not leak
			// "Done...ing 100%..." leftovers.
			name: "carriage return plus erase-in-line drops the uncovered tail",
			in:   "Downloading 100%...\r\x1b[KDone",
			want: "Done",
		},
		{
			name: "carriage return plus explicit 0K erase drops the uncovered tail",
			in:   "Downloading 100%...\r\x1b[0KDone",
			want: "Done",
		},
		{
			// 1K/2K aren't simulated precisely (see eraseLineSentinel),
			// but must still degrade to "no leftover tail", not to
			// garbage.
			name: "carriage return plus erase-to-start (1K) drops the uncovered tail",
			in:   "Downloading 100%...\r\x1b[1KDone",
			want: "Done",
		},
		{
			name: "carriage return plus erase-whole-line (2K) drops the uncovered tail",
			in:   "Downloading 100%...\r\x1b[2KDone",
			want: "Done",
		},
		{
			// The sentinel mechanism must not disturb SGR preservation:
			// the replacement's own color codes survive untouched.
			name: "carriage return plus erase-in-line preserves the replacement's own SGR",
			in:   "\x1b[31mLoading...\x1b[0m\r\x1b[K\x1b[32mDone\x1b[0m",
			want: "\x1b[32mDone\x1b[0m",
		},
		{
			name: "no escapes fast path",
			in:   "just text no escapes",
			want: "just text no escapes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, StripCursorControl(tt.in))
		})
	}
}

// TestSimulateCarriageReturns_PreservesTailAndSGRState is a focused
// regression test for collapseCarriageReturns: the previous
// implementation kept only the text after the last \r, which (a)
// dropped any SGR state set before the \r and (b) erased whatever part
// of the pre-\r text extended past the end of the overwriting text,
// instead of leaving it in place the way a real terminal would.
func TestSimulateCarriageReturns_PreservesTailAndSGRState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "shorter overwrite leaves the uncovered tail visible",
			in:   "0123456789\rabc",
			want: "abc3456789",
		},
		{
			name: "SGR state set before the \\r survives into the tail",
			in:   "\x1b[31mredredred\x1b[0m\rgo",
			want: "go\x1b[31mdredred\x1b[0m",
		},
		{
			name: "overwrite at least as long as the original consumes it entirely",
			in:   "hi\rreplacement",
			want: "replacement",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, simulateCarriageReturns(tt.in))
		})
	}
}

// TestSimulateCarriageReturns_EraseInLineSentinel exercises
// collapseCarriageReturns' handling of eraseLineSentinel directly,
// bypassing StripCursorControl's CSI-stripping pass (which is what
// actually produces the sentinel in production). This isolates the
// \r + erase-in-line interaction from the rest of the pipeline.
func TestSimulateCarriageReturns_EraseInLineSentinel(t *testing.T) {
	t.Parallel()

	sentinel := string(eraseLineSentinel)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "sentinel right after \\r drops the entire uncovered tail",
			in:   "0123456789\r" + sentinel + "abc",
			want: "abc",
		},
		{
			name: "text written before the sentinel within the same segment survives",
			in:   "0123456789\rxy" + sentinel + "z",
			want: "xyz",
		},
		{
			name: "a bare sentinel with no \\r on the line is left untouched here",
			// collapseCarriageReturns only runs on lines that contain a
			// \r; simulateCarriageReturns skips lines without one, so a
			// sentinel with no accompanying \r passes through this
			// function unchanged. StripCursorControl's own trailing
			// cleanup (not simulateCarriageReturns) is what strips it
			// in that case — see TestStripCursorControl's bare "erase
			// line stripped" case.
			in:   "no" + sentinel + "carriage-return",
			want: "no" + sentinel + "carriage-return",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, simulateCarriageReturns(tt.in))
		})
	}
}

func TestRemapANSI16NilColorFallsBack(t *testing.T) {
	t.Parallel()

	var pal [16]color.Color // all nil
	// With a nil palette entry, the introducer is emitted bare so the
	// terminal default applies rather than crashing.
	require.Equal(t, "\x1b[38mx\x1b[0m", RemapANSI16("\x1b[31mx\x1b[0m", pal))
}
