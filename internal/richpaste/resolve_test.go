package richpaste

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
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

// hugeGIFHeaderBytes builds the smallest possible GIF that declares an
// enormous canvas: just the 6-byte signature and the 7-byte logical screen
// descriptor, no color table and no image data at all. image.DecodeConfig
// only needs to read that header to report Width/Height, so this is enough
// to prove the pixel-count check runs before any bitmap is allocated —
// image.Decode would fail on this fixture long before finishing, since
// there is no actual pixel data behind the declared size.
func hugeGIFHeaderBytes(t *testing.T, width, height uint16) []byte {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString("GIF89a")
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, width))
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, height))
	buf.WriteByte(0x00) // no global color table
	buf.WriteByte(0x00) // background color index
	buf.WriteByte(0x00) // pixel aspect ratio
	return buf.Bytes()
}

// TestResolveRefusesImageDeclaringHugePixelCount is defect 1: a small file
// can declare a canvas whose pixel count is bounded only by the format,
// not by the file's own size. normalize must reject it off the declared
// header (image.DecodeConfig) before ever calling image.Decode, which
// would allocate a bitmap sized from that declaration. This fixture is 13
// bytes and declares 65535x65535 (~4.3 billion pixels), so if the check
// ran after decoding instead of before, this test would hang or OOM rather
// than return a fast error.
func TestResolveRefusesImageDeclaringHugePixelCount(t *testing.T) {
	t.Parallel()

	fixture := hugeGIFHeaderBytes(t, 65535, 65535)
	require.Less(t, len(fixture), 32, "fixture must stay tiny to prove the check runs before decode")
	src := "data:image/gif;base64," + base64.StdEncoding.EncodeToString(fixture)

	images, skipped := Resolve(t.Context(), []string{src}, Options{MaxBytes: testMaxBytes})

	require.Empty(t, images)
	require.Equal(t, 1, skipped)
}

// TestResolveAllowsOrdinaryHighDPIScreenshot pins the other side of the
// pixel bound: a real high-DPI screenshot must still resolve, not just get
// refused by an overly tight limit.
func TestResolveAllowsOrdinaryHighDPIScreenshot(t *testing.T) {
	t.Parallel()

	img := image.NewGray(image.Rect(0, 0, 5120, 2880)) // 5K, ~14.7 megapixels.
	var buf bytes.Buffer
	require.NoError(t, gif.Encode(&buf, img, nil))
	src := "data:image/gif;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())

	images, skipped := Resolve(t.Context(), []string{src}, Options{MaxBytes: testMaxBytes})

	require.Zero(t, skipped)
	require.Len(t, images, 1)
	require.Equal(t, "image/png", images[0].MimeType)
}

// TestResolveRefusesLoopbackSource is defect 2's direct case: a source
// that resolves straight to loopback must be refused by the client Resolve
// builds when the caller (the real editor rich-paste path) supplies none.
func TestResolveRefusesLoopbackSource(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(pngBytes(t))
	}))
	defer server.Close()

	images, skipped := Resolve(t.Context(), []string{server.URL + "/img.png"}, Options{MaxBytes: testMaxBytes})

	require.Empty(t, images)
	require.Equal(t, 1, skipped)
}

// TestCheckRedirectHostRefusesLoopbackRedirectTarget is defect 2's redirect
// case. Go's http.Client never calls CheckRedirect for the first request,
// only for each hop after a redirect, so a URL that itself looks fine can
// still redirect somewhere internal — checkRedirectHost is what has to
// catch that. This calls it directly the way net/http would: with the
// would-be next request, whose URL.Host is the redirect target.
//
// This is a focused unit test rather than a live end-to-end redirect,
// because any server this test could actually stand up and connect to
// runs on loopback — so a real round trip cannot show a "safe-looking"
// first hop leading to a "blocked" second one without the first hop being
// blocked too, which is TestResolveRefusesLoopbackSource's case, not this
// one.
func TestCheckRedirectHostRefusesLoopbackRedirectTarget(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:1/redirected.png", nil)
	require.NoError(t, err)

	err = checkRedirectHost(req, nil)

	require.ErrorIs(t, err, errBlockedNetworkAddress)
}

// TestResolveClientAppliesRedirectGuardToCallerSuppliedClient proves that
// guard is actually wired onto the client Resolve uses, end to end: a
// server on loopback (reachable here only because the test hands Resolve
// its own unguarded client, same as the other tests in this file) redirects
// to a second loopback server, and the redirect — not the initial
// request — is what gets refused.
func TestResolveClientAppliesRedirectGuardToCallerSuppliedClient(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(pngBytes(t))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/img.png", http.StatusFound)
	}))
	defer redirector.Close()

	images, skipped := Resolve(t.Context(), []string{redirector.URL + "/start"}, Options{
		MaxBytes: testMaxBytes,
		Client:   redirector.Client(),
	})

	require.Empty(t, images)
	require.Equal(t, 1, skipped)
}

func TestResolveWithoutSourcesReturnsNothing(t *testing.T) {
	t.Parallel()

	images, skipped := Resolve(context.Background(), nil, Options{MaxBytes: testMaxBytes})

	require.Empty(t, images)
	require.Zero(t, skipped)
}
