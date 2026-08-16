// Package genicon renders Sennit's notification icon: a small, deterministic
// PNG built from primitives (image/png, no external assets) rather than a
// hand-drawn design file, so the mark stays in sync with the brand palette
// and can be regenerated on demand
// (`go generate ./internal/ui/notification/...`) without design tooling.
package genicon

import (
	"bytes"
	"cmp"
	"image"
	"image/color"
	"image/png"
	"math"
	"slices"
)

// Size is the width and height, in pixels, of the generated icon.
const Size = 256

// Brand colors, matching styles.SennitDark's BrandPrimary/BrandSecondary/
// BrandAccent tokens and its base background. Hardcoded here rather than
// imported: the styles package pulls in the full TUI theme graph, which
// this small, dependency-free generator has no business depending on.
var (
	colorBackground = color.RGBA{R: 0x15, G: 0x1A, B: 0x20, A: 0xFF} // BrandBgLeast
	colorStrand1    = color.RGBA{R: 0x4A, G: 0x8F, B: 0xA8, A: 0xFF} // BrandPrimary
	colorStrand2    = color.RGBA{R: 0x6E, G: 0xCB, B: 0xD6, A: 0xFF} // BrandSecondary
	colorStrand3    = color.RGBA{R: 0x7F, G: 0xA8, B: 0xC9, A: 0xFF} // BrandKeyword
)

// strand is one ribbon of the flat plait, following a sine path
// y(x) = amplitude*sin(k*x+phase) across the icon.
type strand struct {
	phase float64 // radians, offset of this strand's sine path.
	color color.RGBA
}

// sineSamples is the number of straight segments used to approximate each
// strand's sine curve. High enough that the piecewise-capsule approximation
// is visually indistinguishable from a true curve at this resolution.
const sineSamples = 48

// Render draws the Sennit notification icon: a dark rounded square with a
// flat three-strand plait — three ribbons weaving over and under each other
// along a common sine axis, in the brand's primary/secondary/accent colors.
// It carries no text so it stays legible at notification-icon sizes
// (~48px). Render is pure and deterministic — the same bytes come out every
// call — so the committed asset in ../assets can be verified against it in
// tests.
func Render() []byte {
	const (
		half      = Size / 2
		radius    = 44.0                     // corner radius of the background square.
		aa        = 1.5                      // anti-aliasing band width, in pixels.
		amplitude = 54.0                     // peak height of each ribbon's sine path, in pixels.
		ribbonW   = 18.0                     // half-width of each ribbon.
		k         = 2 * math.Pi * 1.5 / Size // 1.5 periods across the icon, for several crossings.
	)

	// Phases are spaced 120° apart, the standard flat-braid construction:
	// three strands sharing one sine axis, offset so each takes its turn on
	// top as x advances.
	strands := []strand{
		{phase: 0, color: colorStrand1},
		{phase: 2 * math.Pi / 3, color: colorStrand2},
		{phase: 4 * math.Pi / 3, color: colorStrand3},
	}

	img := image.NewNRGBA(image.Rect(0, 0, Size, Size))
	for y := range Size {
		for x := range Size {
			px, py := float64(x)+0.5-half, float64(y)+0.5-half

			bgAlpha := coverage(roundedRectSDF(px, py, half, half, radius), aa)

			// Depth at this pixel's x for each strand: z_i(x) = cos(k*x+phase_i).
			// Compositing back-to-front by z (rather than in a fixed order) is
			// what turns three overlapping ribbons into a genuine over-under
			// weave.
			order := make([]int, len(strands))
			for i := range order {
				order[i] = i
			}
			slices.SortFunc(order, func(a, b int) int {
				za := math.Cos(k*px + strands[a].phase)
				zb := math.Cos(k*px + strands[b].phase)
				return cmp.Compare(za, zb)
			})

			r, g, b := float64(colorBackground.R), float64(colorBackground.G), float64(colorBackground.B)
			for _, i := range order {
				s := strands[i]
				t := coverage(sineRibbonSDF(px, py, k, amplitude, s.phase, ribbonW), aa)
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

// sineRibbonSDF returns the signed distance from (px,py) to a ribbon of the
// given half-width following y = amplitude*sin(k*x+phase), approximated by
// sampling the curve into sineSamples straight segments and taking the
// minimum capsule distance over them.
func sineRibbonSDF(px, py, k, amplitude, phase, halfWidth float64) float64 {
	const half = Size / 2

	sampleX := func(i int) float64 {
		return -half + float64(i)*Size/float64(sineSamples)
	}
	sampleY := func(x float64) float64 {
		return amplitude * math.Sin(k*x+phase)
	}

	min := math.Inf(1)
	ax, ay := sampleX(0), sampleY(sampleX(0))
	for i := 1; i <= sineSamples; i++ {
		bx, by := sampleX(i), sampleY(sampleX(i))
		if d := capsuleSDF(px, py, ax, ay, bx, by, halfWidth); d < min {
			min = d
		}
		ax, ay = bx, by
	}
	return min
}

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
// segment from (ax,ay) to (bx,by), stroked with round caps) of the given
// half-width.
func capsuleSDF(px, py, ax, ay, bx, by, halfWidth float64) float64 {
	ex, ey := bx-ax, by-ay
	wx, wy := px-ax, py-ay
	t := clamp01((wx*ex + wy*ey) / (ex*ex + ey*ey))
	cx, cy := ax+t*ex, ay+t*ey
	return math.Hypot(px-cx, py-cy) - halfWidth
}
