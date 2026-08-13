package image

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"io"

	"golang.org/x/image/draw"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/disintegration/imaging"
)

type Encoding byte

const (
	EncodingBlocks Encoding = iota
	EncodingKitty
)

type CellSize struct {
	Width, Height int
}

type PreviewKey struct {
	Path            string
	Size            int64
	ModTimeUnixNano int64
	Columns         int
	Rows            int
	Encoding        Encoding
	CellSize        CellSize
	Tmux            bool
}

type PreviewPreparedMsg struct {
	Instance   uint64
	Key        PreviewKey
	Generation uint64
	Rendered   string
	Output     string
	Err        error
}

func ResetCache() {}

func (k PreviewKey) ID() string {
	return fmt.Sprintf("%s-%d-%d-%dx%d-%d-%dx%d-%t", k.Path, k.Size, k.ModTimeUnixNano, k.Columns, k.Rows, k.Encoding, k.CellSize.Width, k.CellSize.Height, k.Tmux)
}

func (k PreviewKey) hash() uint32 {
	h := fnv.New32a()
	_, _ = io.WriteString(h, k.ID())
	return h.Sum32()
}

func Prepare(key PreviewKey, img image.Image) (string, string, error) {
	if img == nil {
		return "", "", fmt.Errorf("image is nil")
	}
	if key.Columns <= 0 || key.Rows <= 0 {
		return "", "", nil
	}
	img = fitImage(img, key.CellSize, key.Columns, key.Rows)
	switch key.Encoding {
	case EncodingBlocks:
		return paint(img, key.Columns, key.Rows), "", nil
	case EncodingKitty:
		var output bytes.Buffer
		bounds := img.Bounds()
		if err := kitty.EncodeGraphics(&output, img, &kitty.Options{
			ID:               int(key.hash()),
			Action:           kitty.TransmitAndPut,
			Transmission:     kitty.Direct,
			Format:           kitty.RGBA,
			ImageWidth:       bounds.Dx(),
			ImageHeight:      bounds.Dy(),
			Columns:          key.Columns,
			Rows:             key.Rows,
			VirtualPlacement: true,
			Quite:            1,
			Chunk:            true,
			ChunkFormatter: func(chunk string) string {
				if key.Tmux {
					return ansi.TmuxPassthrough(chunk)
				}
				return chunk
			},
		}); err != nil {
			return "", "", fmt.Errorf("encode kitty graphics: %w", err)
		}
		return kittyPlaceholder(key), output.String(), nil
	default:
		return "", "", fmt.Errorf("unsupported image encoding")
	}
}

func fitImage(img image.Image, cellSize CellSize, columns, rows int) image.Image {
	if cellSize.Width <= 0 || cellSize.Height <= 0 {
		return img
	}
	return imaging.Fit(img, columns*cellSize.Width, rows*cellSize.Height, imaging.Lanczos)
}

func paint(img image.Image, columns, rows int) string {
	bounds := img.Bounds()
	canvas := image.NewRGBA(image.Rect(0, 0, columns, rows))
	draw.ApproxBiLinear.Scale(canvas, canvas.Bounds(), img, bounds, draw.Over, nil)
	var output bytes.Buffer
	for y := range rows {
		for x := range columns {
			red, green, blue, _ := canvas.At(x, y).RGBA()
			fmt.Fprintf(&output, "\x1b[48;2;%d;%d;%dm ", red>>8, green>>8, blue>>8)
		}
		output.WriteString("\x1b[0m")
		if y < rows-1 {
			output.WriteByte('\n')
		}
	}
	return output.String()
}

func kittyPlaceholder(key PreviewKey) string {
	id := int(key.hash())
	extra, r, g, b := id>>24&0xff, id>>16&0xff, id>>8&0xff, id&0xff
	var foreground color.Color
	if id <= 255 {
		foreground = ansi.IndexedColor(b)
	} else {
		foreground = color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff}
	}
	style := ansi.NewStyle().ForegroundColor(foreground).String()
	var output bytes.Buffer
	for y := range key.Rows {
		output.WriteString(style)
		output.WriteRune(kitty.Placeholder)
		output.WriteRune(kitty.Diacritic(y))
		output.WriteRune(kitty.Diacritic(0))
		if extra > 0 {
			output.WriteRune(kitty.Diacritic(extra))
		}
		for x := 1; x < key.Columns; x++ {
			output.WriteString(style)
			output.WriteRune(kitty.Placeholder)
		}
		if y < key.Rows-1 {
			output.WriteByte('\n')
		}
	}
	return output.String()
}
