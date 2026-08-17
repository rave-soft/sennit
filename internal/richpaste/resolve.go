package richpaste

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/sync/errgroup"

	// Registered so that Resolve can re-encode the formats browsers serve
	// but the models do not take.
	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/webp"
)

// fetchConcurrency bounds how many images are downloaded at once.
const fetchConcurrency = 4

// whitespace strips the line breaks markup inserts into base64 payloads.
var whitespace = strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "")

// Image is one resolved clipboard image, normalized to a format the models
// accept (PNG or JPEG).
type Image struct {
	Content  []byte
	MimeType string
}

// Options configures Resolve.
type Options struct {
	// Client fetches http(s) sources. Defaults to http.DefaultClient.
	Client *http.Client
	// MaxBytes is the per-image size ceiling. Bigger images are skipped.
	MaxBytes int64
}

// Resolve reads every source it can — data: URIs inline, http(s) URLs over
// the network — and returns the resulting images in source order together
// with the number of sources it had to skip (unsupported scheme, too big,
// undecodable, or a failed request).
func Resolve(ctx context.Context, srcs []string, opts Options) (images []Image, skipped int) {
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}

	resolved := make([]*Image, len(srcs))
	var group errgroup.Group
	group.SetLimit(fetchConcurrency)
	for i, src := range srcs {
		group.Go(func() error {
			img, err := resolveOne(ctx, src, opts)
			if err != nil {
				// One bad image must not sink the rest of the paste.
				return nil
			}
			resolved[i] = img
			return nil
		})
	}
	_ = group.Wait()

	for _, img := range resolved {
		if img == nil {
			skipped++
			continue
		}
		images = append(images, *img)
	}
	return images, skipped
}

func resolveOne(ctx context.Context, src string, opts Options) (*Image, error) {
	var (
		content []byte
		err     error
	)
	switch {
	case strings.HasPrefix(src, "data:"):
		content, err = decodeDataURI(src)
	default:
		content, err = download(ctx, src, opts)
	}
	if err != nil {
		return nil, err
	}
	return normalize(content, opts.MaxBytes)
}

// decodeDataURI reads the payload of a data: URI, base64-encoded or percent-
// encoded.
func decodeDataURI(src string) ([]byte, error) {
	header, payload, found := strings.Cut(src, ",")
	if !found {
		return nil, fmt.Errorf("malformed data URI")
	}
	if strings.Contains(header, ";base64") {
		// Markup wraps long payloads across lines; the decoder will not.
		payload = whitespace.Replace(payload)
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return base64.RawStdEncoding.DecodeString(payload)
		}
		return decoded, nil
	}
	decoded, err := url.PathUnescape(payload)
	if err != nil {
		return nil, err
	}
	return []byte(decoded), nil
}

// download fetches an http(s) source, refusing anything past MaxBytes rather
// than buffering it. Relative URLs are unusable: the clipboard carries no
// base to resolve them against.
func download(ctx context.Context, src string, opts Options) ([]byte, error) {
	u, err := url.Parse(src)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported image source scheme %q", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := opts.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", u.Redacted(), resp.Status)
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, opts.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > opts.MaxBytes {
		return nil, fmt.Errorf("image at %s is too big", u.Redacted())
	}
	return content, nil
}

// normalize keeps PNG and JPEG as they are and re-encodes everything else
// (WebP, GIF) to PNG, since that is what the models take.
func normalize(content []byte, maxBytes int64) (*Image, error) {
	if len(content) == 0 {
		return nil, fmt.Errorf("empty image")
	}

	mimeType := http.DetectContentType(content[:min(512, len(content))])
	if mimeType == "image/png" || mimeType == "image/jpeg" {
		return &Image{Content: content, MimeType: mimeType}, nil
	}

	decoded, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, decoded); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > maxBytes {
		return nil, fmt.Errorf("re-encoded image is too big")
	}
	return &Image{Content: buf.Bytes(), MimeType: "image/png"}, nil
}
