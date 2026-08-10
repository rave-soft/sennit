// Package genicon renders Braid's notification icon: a small, deterministic
// PNG built from primitives (image/png, no external assets) rather than a
// hand-drawn design file, so the mark stays in sync with the brand palette
// and can be regenerated on demand
// (`go generate ./internal/ui/notification/...`) without design tooling.
package genicon

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Size is the width and height, in pixels, of the generated icon.
const Size = 256

// Brand colors, matching styles.CharmtonePantera's primary/secondary/accent
// tokens (charmtone.Charple/Dolly/Bok) and its dark base background
// (charmtone.Pepper). Hardcoded here rather than imported: the styles
// package pulls in the full TUI theme graph, which this small,
// dependency-free generator has no business depending on.
var (
	colorBackground = color.RGBA{R: 0x20, G: 0x1F, B: 0x26, A: 0xFF} // Pepper
	colorStrand1    = color.RGBA{R: 0x6B, G: 0x50, B: 0xFF, A: 0xFF} // Charple (primary)
	colorStrand2    = color.RGBA{R: 0xFF, G: 0x60, B: 0xFF, A: 0xFF} // Dolly (secondary)
	colorStrand3    = color.RGBA{R: 0x68, G: 0xFF, B: 0xD6, A: 0xFF} // Bok (accent)
)

// strand is one diagonal band of the braid mark.
type strand struct {
	angle float64 // radians, measured from the x-axis.
	color color.RGBA
}

// Render draws the Braid notification icon: a dark rounded square with
// three crossing diagonal strands in the brand's primary/secondary/accent
// colors, evoking a braid/plait. It carries no text so it stays legible at
// notification-icon sizes (~48px). Render is pure and deterministic — the
// same bytes come out every call — so the committed asset in ../assets can
// be verified against it in tests.
func Render() []byte {
	const (
		half      = Size / 2
		radius    = 44.0  // corner radius of the background square.
		aa        = 1.5   // anti-aliasing band width, in pixels.
		strandLen = 200.0 // half-length of each strand capsule.
		strandW   = 26.0  // half-width of each strand capsule.
	)

	// Angles are spaced 60° apart and offset from the axes so all three
	// strands read as diagonal, none landing on a horizontal or vertical.
	strands := []strand{
		{angle: deg(20), color: colorStrand1},
		{angle: deg(80), color: colorStrand2},
		{angle: deg(140), color: colorStrand3},
	}

	img := image.NewNRGBA(image.Rect(0, 0, Size, Size))
	for y := range Size {
		for x := range Size {
			px, py := float64(x)+0.5-half, float64(y)+0.5-half

			bgAlpha := coverage(roundedRectSDF(px, py, half, half, radius), aa)

			r, g, b := float64(colorBackground.R), float64(colorBackground.G), float64(colorBackground.B)
			for _, s := range strands {
				t := coverage(capsuleSDF(px, py, s.angle, strandLen, strandW), aa)
				r = lerp(r, float64(s.color.R), t)
				g = lerp(g, float64(s.color.G), t)
				b = lerp(b, float64(s.color.B), t)
			}

			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(math.Round(r)),
				G: uint8(math.Round(g)),
				B: uint8(math.Round(b)),
				A: uint8(math.Round(bgAlpha * 255)),
			})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		// Encoding a freshly built in-memory NRGBA image cannot fail.
		panic(err)
	}
	return buf.Bytes()
}

func deg(d float64) float64 { return d * math.Pi / 180 }

// coverage converts a signed distance (negative inside a shape, positive
// outside) into an anti-aliased [0,1] coverage value over a band of width aa
// centered on the shape's edge.
func coverage(sdf, aa float64) float64 {
	return clamp01(0.5 - sdf/aa)
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

// roundedRectSDF returns the signed distance from (px,py) to the boundary of
// a rounded rectangle of the given half-width/half-height centered at the
// origin (the standard rounded-box signed distance function).
func roundedRectSDF(px, py, halfW, halfH, radius float64) float64 {
	dx := math.Abs(px) - (halfW - radius)
	dy := math.Abs(py) - (halfH - radius)
	ax, ay := math.Max(dx, 0), math.Max(dy, 0)
	outside := math.Hypot(ax, ay) - radius
	inside := min(math.Max(dx, dy), 0)
	return outside + inside
}

// capsuleSDF returns the signed distance from (px,py) to a capsule (a line
// segment stroked with round caps) of the given half-length/half-width,
// centered at the origin at the given angle.
func capsuleSDF(px, py, angle, halfLen, halfWidth float64) float64 {
	cos, sin := math.Cos(angle), math.Sin(angle)
	lx := px*cos + py*sin
	ly := -px*sin + py*cos
	qx := math.Max(math.Abs(lx)-halfLen, 0)
	return math.Hypot(qx, ly) - halfWidth
}
