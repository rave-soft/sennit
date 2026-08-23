package richpaste

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

const testMaxBytes = 5 * 1024 * 1024

func pngBytes(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func gifBytes(t *testing.T) []byte {
	t.Helper()

	img := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	require.NoError(t, gif.Encode(&buf, img, nil))
	return buf.Bytes()
}

func TestResolveReadsBase64DataURI(t *testing.T) {
	t.Parallel()

	content := pngBytes(t)
	src := "data:image/png;base64," + base64.StdEncoding.EncodeToString(content)

	images, skipped := Resolve(t.Context(), []string{src}, Options{MaxBytes: testMaxBytes})

	require.Zero(t, skipped)
	require.Len(t, images, 1)
	require.Equal(t, "image/png", images[0].MimeType)
	require.Equal(t, content, images[0].Content)
}

func TestResolveReadsDataURIWrappedAcrossLines(t *testing.T) {
	t.Parallel()

	encoded := base64.StdEncoding.EncodeToString(pngBytes(t))
	src := "data:image/png;base64," + encoded[:10] + "\n  " + encoded[10:]

	images, skipped := Resolve(t.Context(), []string{src}, Options{MaxBytes: testMaxBytes})

	require.Zero(t, skipped)
	require.Len(t, images, 1)
}

func TestResolveDownloadsHTTPSourcesInOrder(t *testing.T) {
	t.Parallel()

	content := pngBytes(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.png" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()

	images, skipped := Resolve(t.Context(), []string{
		server.URL + "/one.png",
		server.URL + "/missing.png",
		server.URL + "/two.png",
	}, Options{MaxBytes: testMaxBytes, Client: server.Client()})

	require.Equal(t, 1, skipped)
	require.Len(t, images, 2)
	for _, img := range images {
		require.Equal(t, content, img.Content)
	}
}

func TestResolveReEncodesFormatsModelsDoNotTake(t *testing.T) {
	t.Parallel()

	src := "data:image/gif;base64," + base64.StdEncoding.EncodeToString(gifBytes(t))

	images, skipped := Resolve(t.Context(), []string{src}, Options{MaxBytes: testMaxBytes})

	require.Zero(t, skipped)
	require.Len(t, images, 1)
	require.Equal(t, "image/png", images[0].MimeType)
	_, err := png.Decode(bytes.NewReader(images[0].Content))
	require.NoError(t, err)
}

// TestResolveSkipsOversizedDataURI guards MaxAttachmentSize against a
// base64 data: URI, not just http(s) downloads. download enforces MaxBytes
// with a LimitReader while streaming, but a data: URI is already fully in
// memory as one string with no equivalent check, so an oversized inline
// image used to sail straight through.
func TestResolveSkipsOversizedDataURI(t *testing.T) {
	t.Parallel()

	content := pngBytes(t)
	src := "data:image/png;base64," + base64.StdEncoding.EncodeToString(content)

	images, skipped := Resolve(t.Context(), []string{src}, Options{MaxBytes: int64(len(content) - 1)})

	require.Empty(t, images)
	require.Equal(t, 1, skipped)
}

func TestResolveSkipsOversizedAndUnusableSources(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 128))
	}))
	defer server.Close()

	images, skipped := Resolve(t.Context(), []string{
		server.URL + "/big.png",
		"/relative/path.png",
		"file:///etc/passwd",
		"data:image/png;base64,not-an-image",
	}, Options{MaxBytes: 32, Client: server.Client()})

	require.Empty(t, images)
	require.Equal(t, 4, skipped)
}

func TestResolveWithoutSourcesReturnsNothing(t *testing.T) {
	t.Parallel()

	images, skipped := Resolve(context.Background(), nil, Options{MaxBytes: testMaxBytes})

	require.Empty(t, images)
	require.Zero(t, skipped)
}
