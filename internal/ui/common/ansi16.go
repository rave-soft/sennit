package common

import (
	"image/color"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// RemapANSI16 replaces basic ANSI 16-color SGR codes with 24-bit
// truecolor from palette. Programs emit \x1b[31m etc. and trust the
// terminal to pick the color; inside Sennit's TUI those defaults are
// often illegible on our dark background. Rewriting them to explicit
// RGB keeps output readable regardless of terminal configuration.
//
// Uses [ansi.DecodeSequence] for parsing (same approach as
// [colorprofile.Writer]) since there is no upstream palette-remap API.
func RemapANSI16(s string, palette [16]color.Color) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}

	var buf strings.Builder
	buf.Grow(len(s))

	parser := ansi.GetParser()
	defer ansi.PutParser(parser)

	var state byte
	for len(s) > 0 {
		parser.Reset()
		seq, _, n, newState := ansi.DecodeSequence(s, state, parser)

		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			remapSGR(parser.Params(), palette, &buf)
		} else {
			buf.WriteString(seq)
		}

		s = s[n:]
		state = newState
	}

	return buf.String()
}

// remapSGR rewrites one SGR sequence, replacing 16-color params with
// truecolor from palette. Extended colors (38/48/58 with ;5;n or
// ;2;r;g;b sub-params) pass through unchanged. Non-color attributes
// (bold, italic, etc.) and default color resets (39/49/59) also pass
// through.
func remapSGR(params ansi.Params, palette [16]color.Color, buf *strings.Builder) {
	buf.WriteString("\x1b[")

	first := true
	for i := 0; i < len(params); i++ {
		p := params[i].Param(0)

		if !first {
			buf.WriteByte(';')
		}
		first = false

		switch {
		// Extended color introducers consume subsequent params as
		// arguments. Skip them whole so they aren't misread.
		case p == 38 || p == 48 || p == 58:
			buf.WriteString(strconv.Itoa(p))
			if i+1 < len(params) {
				sub := params[i+1].Param(0)
				switch sub {
				case 5: // 256-color: 38;5;n
					buf.WriteByte(';')
					buf.WriteString(strconv.Itoa(sub))
					if i+2 < len(params) {
						buf.WriteByte(';')
						buf.WriteString(strconv.Itoa(params[i+2].Param(0)))
						i += 2
					} else {
						i++
					}
				case 2: // truecolor: 38;2;r;g;b
					buf.WriteByte(';')
					buf.WriteString(strconv.Itoa(sub))
					for j := 2; j <= 4 && i+j < len(params); j++ {
						buf.WriteByte(';')
						buf.WriteString(strconv.Itoa(params[i+j].Param(0)))
					}
					i += min(4, len(params)-i-1)
				default:
					i++
				}
			}

		case p >= 30 && p <= 37:
			writeTruecolor(buf, 38, palette[p-30])
		case p >= 90 && p <= 97:
			writeTruecolor(buf, 38, palette[8+p-90])
		case p >= 40 && p <= 47:
			writeTruecolor(buf, 48, palette[p-40])
		case p >= 100 && p <= 107:
			writeTruecolor(buf, 48, palette[8+p-100])

		default:
			buf.WriteString(strconv.Itoa(p))
		}
	}

	buf.WriteByte('m')
}

// eraseLineSentinel marks where a CSI K (erase-in-line) sequence occurred,
// so the \r-collapsing pass (which runs after this scan is done and the
// escape codes are gone) can still see it and react. It's a Unicode
// Private Use Area code point, which real programs never emit as visible
// text, so it can't collide with genuine content. Any sentinel left over
// once both passes are done (bare CSI K with no \r on the same line) is
// stripped at the very end of StripCursorControl — that's the same
// "no visible effect" outcome K always had here, just routed through the
// sentinel instead of being dropped inline.
const eraseLineSentinel = '\uE000'

// StripCursorControl removes ANSI escape sequences that move the cursor,
// erase regions of the screen, or change terminal modes. These sequences
// are emitted by programs like git push, cargo build, and npm install to
// animate progress bars and status lines. When captured as raw text and
// replayed inside Sennit's TUI viewport they corrupt the render state.
//
// Preserved: SGR (color/style) sequences, OSC hyperlinks, printable text.
// Stripped: CSI cursor movement (A-H, f), erase display (J), scroll
// (S, T), save/restore cursor (s, u), DEC private modes (?h, ?l), and the
// ESC save/restore cursor sequences (ESC 7, ESC 8). Bare carriage returns
// (\r) are handled by simulating line-overwrite behavior: within each
// line, text after the last \r wins, matching what a real terminal would
// display, and any SGR state or uncovered tail from before the \r
// survives if nothing erases it (see collapseCarriageReturns).
//
// CSI K (erase-in-line) is not simply dropped: combined with \r —
// "...\r\x1b[K<replacement>", the standard way progress bars clear a
// line before redrawing it — the erase changes what \r-collapsing
// should keep. K is replaced with [eraseLineSentinel] here so the later
// pass can act on it; see collapseCarriageReturns for what the sentinel
// means once there.
func StripCursorControl(s string) string {
	if !strings.ContainsRune(s, 0x1b) && !strings.ContainsRune(s, '\r') {
		return s
	}

	var buf strings.Builder
	buf.Grow(len(s))

	parser := ansi.GetParser()
	defer ansi.PutParser(parser)

	var state byte
	for len(s) > 0 {
		parser.Reset()
		seq, _, n, newState := ansi.DecodeSequence(s, state, parser)

		if ansi.HasCsiPrefix(seq) {
			cmd := parser.Command()
			final := cmd & 0xff
			prefix := (cmd >> 8) & 0xff

			switch final {
			case 'm':
				// SGR: keep (colors/styles).
				buf.WriteString(seq)
			case 'h', 'l':
				// DEC private mode set/reset (?h, ?l): strip.
				// Regular h/l without ? prefix are also non-rendering.
				_ = prefix
			case 'K':
				// Erase-in-line: leave a sentinel instead of dropping
				// it outright — see eraseLineSentinel and
				// collapseCarriageReturns. We don't distinguish
				// 0K/1K/2K (erase right/left/whole); all three land on
				// the same sentinel, which collapseCarriageReturns
				// treats as "erase to end of line". That's exact for
				// the overwhelmingly common 0K-after-\r case and a
				// deliberately conservative approximation for 1K/2K —
				// it only ever discards more of the stale previous
				// line than a real terminal would, never less, so it
				// can't leak leftover text the way dropping K outright
				// did.
				buf.WriteRune(eraseLineSentinel)
			case 'A', 'B', 'C', 'D', // cursor up/down/forward/back
				'E', 'F', // cursor next/prev line
				'G',      // cursor horizontal absolute
				'H', 'f', // cursor position
				'J',      // erase display
				'S', 'T', // scroll up/down
				's', 'u': // save/restore cursor
				// Strip all cursor/screen control.
			default:
				// Unknown CSI: pass through to avoid data loss.
				buf.WriteString(seq)
			}
		} else if ansi.HasEscPrefix(seq) && len(seq) == 2 {
			// ESC followed by single byte: check for DEC save/restore.
			switch seq[1] {
			case '7', '8':
				// DEC save/restore cursor: strip.
			default:
				buf.WriteString(seq)
			}
		} else {
			buf.WriteString(seq)
		}

		s = s[n:]
		state = newState
	}

	result := buf.String()

	// Handle bare \r by simulating line-overwrite. Split on newlines
	// first so we only process \r within individual lines.
	if strings.ContainsRune(result, '\r') {
		result = simulateCarriageReturns(result)
	}

	// Any sentinel collapseCarriageReturns didn't consume — a CSI K with
	// no \r on the same line to pair it with — never had a visible
	// effect here to begin with (K alone, without first returning the
	// cursor somewhere, erases nothing that isn't about to be
	// overwritten by the very next characters). Drop it rather than
	// leak the marker into the rendered output.
	if strings.ContainsRune(result, eraseLineSentinel) {
		result = strings.ReplaceAll(result, string(eraseLineSentinel), "")
	}

	return result
}

// simulateCarriageReturns processes bare \r characters within each line,
// simulating terminal overwrite behavior. Progress bars use this pattern
// extensively.
func simulateCarriageReturns(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.ContainsRune(line, '\r') {
			lines[i] = collapseCarriageReturns(line)
		}
	}
	return strings.Join(lines, "\n")
}

// collapseCarriageReturns resolves the \r's within a single line into the
// text a real terminal would show. \r only moves the cursor back to
// column 0 — it neither resets SGR state nor erases the rest of the line —
// so the text after it overwrites just the columns it covers, and
// whatever color/style was active before the \r keeps applying to
// anything beyond that.
//
// A naive "keep only the text after the last \r" (the previous behavior
// here) gets both of those wrong: it drops any SGR codes set earlier on
// the line, and it drops the tail of the line past the end of the
// overwriting text instead of leaving it visible.
func collapseCarriageReturns(line string) string {
	idx := strings.LastIndex(line, "\r")
	if idx < 0 {
		return line
	}
	after := line[idx+1:]

	if strings.ContainsRune(after, eraseLineSentinel) {
		// The overwhelmingly common progress-bar pattern is
		// "...\r\x1b[K<replacement>": return to column 0, erase
		// whatever was on the line, then draw the replacement. An
		// erase sentinel anywhere in `after` means the terminal
		// explicitly cleared the line before continuing, so — unlike
		// the plain-\r case below — nothing from `before` survives
		// here regardless of width. `before` is never even resolved:
		// it would only be thrown away.
		return strings.ReplaceAll(after, string(eraseLineSentinel), "")
	}

	// Resolve any earlier \r's on this line first, left to right.
	before := collapseCarriageReturns(line[:idx])

	// TruncateLeft drops the leading cells `after` overwrites, but (being
	// ANSI-aware) still copies forward every escape sequence it passes
	// over on the way — which is exactly the SGR state that was active
	// at that column, plus the surviving tail of `before` beyond what
	// `after` covers.
	return after + ansi.TruncateLeft(before, ansi.StringWidth(after), "")
}

// writeTruecolor appends "introducer;2;r;g;b" to buf. Nil color emits
// the bare introducer so the terminal default applies.
func writeTruecolor(buf *strings.Builder, introducer int, c color.Color) {
	if c == nil {
		buf.WriteString(strconv.Itoa(introducer))
		return
	}
	r, g, b, _ := c.RGBA()
	buf.WriteString(strconv.Itoa(introducer))
	buf.WriteString(";2;")
	buf.WriteString(strconv.Itoa(int(r >> 8)))
	buf.WriteByte(';')
	buf.WriteString(strconv.Itoa(int(g >> 8)))
	buf.WriteByte(';')
	buf.WriteString(strconv.Itoa(int(b >> 8)))
}
