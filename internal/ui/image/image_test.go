package image

import (
	"image"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreviewKeyIncludesAllInputs(t *testing.T) {
	key := PreviewKey{Path: "a.png", Size: 10, ModTimeUnixNano: 11, Columns: 12, Rows: 13, Encoding: EncodingKitty, CellSize: CellSize{Width: 8, Height: 16}, Tmux: true}
	other := key
	other.ModTimeUnixNano++
	require.NotEqual(t, key.ID(), other.ID())
	other = key
	other.Encoding = EncodingBlocks
	require.NotEqual(t, key.ID(), other.ID())
}

func TestPrepareKittyUsesCanonicalID(t *testing.T) {
	key := PreviewKey{Path: "a.png", Size: 10, ModTimeUnixNano: 11, Columns: 2, Rows: 1, Encoding: EncodingKitty, CellSize: CellSize{Width: 8, Height: 16}}
	rendered, output, err := Prepare(key, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	require.NoError(t, err)
	require.NotEmpty(t, output)
	require.NotEmpty(t, rendered)
	require.Equal(t, kittyPlaceholder(key), rendered)
}

func TestPrepareBlocksIsNonEmpty(t *testing.T) {
	key := PreviewKey{Path: "a.png", Columns: 2, Rows: 1, Encoding: EncodingBlocks}
	rendered, output, err := Prepare(key, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	require.NoError(t, err)
	require.NotEmpty(t, rendered)
	require.Empty(t, output)
}

func TestPrepareBlocksDoesNotUsePaintbrush(t *testing.T) {
	key := PreviewKey{Columns: 2, Rows: 1, Encoding: EncodingBlocks}
	rendered, _, err := Prepare(key, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	require.NoError(t, err)
	require.Contains(t, rendered, "\x1b[48;2;")
}
