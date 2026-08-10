package genicon_test

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/rave-soft/braid/internal/ui/notification"
	"github.com/rave-soft/braid/internal/ui/notification/genicon"
	"github.com/stretchr/testify/require"
)

// TestRender_Deterministic pins that Render is a pure function of its
// (hardcoded) inputs: regenerating the icon must never produce a diff, since
// the committed asset is expected to be exactly what `go generate` produces.
func TestRender_Deterministic(t *testing.T) {
	t.Parallel()

	first := genicon.Render()
	second := genicon.Render()
	require.Equal(t, first, second, "Render must be deterministic across calls")
}

// TestRender_ValidPNG checks the rendered bytes decode as a PNG of the
// expected size with an alpha channel (the rounded corners rely on it).
func TestRender_ValidPNG(t *testing.T) {
	t.Parallel()

	img, err := png.Decode(bytes.NewReader(genicon.Render()))
	require.NoError(t, err)

	bounds := img.Bounds()
	require.Equal(t, genicon.Size, bounds.Dx())
	require.Equal(t, genicon.Size, bounds.Dy())

	// Corners must be transparent (rounded corner cutout); center must be
	// opaque (background fill).
	_, _, _, cornerAlpha := img.At(0, 0).RGBA()
	require.Zero(t, cornerAlpha, "corner pixel should be fully transparent")

	_, _, _, centerAlpha := img.At(genicon.Size/2, genicon.Size/2).RGBA()
	require.NotZero(t, centerAlpha, "center pixel should be opaque")
}

// TestRender_MatchesCommittedAsset guards against the checked-in
// internal/ui/notification/assets/braid.png going stale relative to the
// generator: if this fails, someone edited icon.go without running
// `go generate ./internal/ui/notification/...`.
func TestRender_MatchesCommittedAsset(t *testing.T) {
	t.Parallel()

	require.Equal(t, genicon.Render(), notification.Icon,
		"assets/braid.png is stale; run `go generate ./internal/ui/notification/...`")
}
