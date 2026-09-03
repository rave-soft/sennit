package richpaste

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	// Registered so that Resolve can re-encode the formats browsers serve
	// but the models do not take.
	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/webp"
)

// fetchConcurrency bounds how many images are downloaded at once.
const fetchConcurrency = 4

// maxImagePixels bounds the pixel count Sennit will decode a pasted image
// into before checking its re-encoded size. A well-compressed file can
// declare a canvas far larger than its byte size suggests, so this must be
// checked from the header alone (image.DecodeConfig), before image.Decode
// allocates a bitmap sized off that declaration. A 6K high-DPI screenshot
// (6016x3384) is ~20 million pixels; 64 million comfortably covers wider
// multi-monitor or pixel-doubled captures while still refusing a small
// file that declares a canvas sized only to exhaust memory.
const maxImagePixels = 64_000_000

// errBlockedNetworkAddress explains why an image source was refused for
// resolving to a network address Sennit will not fetch from — loopback,
// private, link-local, or unspecified. Pasted markup is attacker-controlled
// (it can be copied from any web page), so it must not be able to make
// this process reach its own host or local network, including by
// redirecting a request there after the initial URL looked fine.
var errBlockedNetworkAddress = errors.New("refusing to fetch from a private, loopback, or link-local address")

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
		// The default client gets both layers of protection: a dialer that
		// refuses to connect to a blocked address at all, and a redirect
		// check. A caller-supplied client (tests, mainly) only gets the
		// redirect check layered on top of whatever transport it already
		// has — see withRedirectGuard.
		opts.Client = defaultRichPasteClient()
	} else {
		opts.Client = withRedirectGuard(opts.Client)
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
		content, err = decodeDataURI(src, opts.MaxBytes)
	default:
		content, err = download(ctx, src, opts)
	}
	if err != nil {
		return nil, err
	}
	return normalize(content, opts.MaxBytes)
}

// decodeDataURI reads the payload of a data: URI, base64-encoded or percent-
// encoded, refusing anything past maxBytes once decoded — a data: URI is
// already fully in memory as one string, unlike download's streamed fetch,
// but it must be held to the same MaxAttachmentSize ceiling.
func decodeDataURI(src string, maxBytes int64) ([]byte, error) {
	header, payload, found := strings.Cut(src, ",")
	if !found {
		return nil, fmt.Errorf("malformed data URI")
	}
	var (
		decoded []byte
		err     error
	)
	if strings.Contains(header, ";base64") {
		// Markup wraps long payloads across lines; the decoder will not.
		payload = whitespace.Replace(payload)
		decoded, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(payload)
		}
	} else {
		var s string
		s, err = url.PathUnescape(payload)
		decoded = []byte(s)
	}
	if err != nil {
		return nil, err
	}
	if int64(len(decoded)) > maxBytes {
		return nil, fmt.Errorf("data URI image is too big")
	}
	return decoded, nil
}

// defaultRichPasteClient is what Resolve uses when the caller (in practice,
// the editor's rich-paste path) supplies no client of its own. It is the
// client that ever sees an unconfirmed URL pulled straight out of pasted
// markup, so it is the one that must not be tricked into reaching an
// internal address.
func defaultRichPasteClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = blockedAddrDialContext(&net.Dialer{Timeout: 10 * time.Second})
	return &http.Client{
		Transport:     transport,
		CheckRedirect: checkRedirectHost,
	}
}

// withRedirectGuard adds the redirect check to a caller-supplied client
// without touching its transport, so tests that hand Resolve an
// httptest.Server's client keep dialing loopback the way they always have.
// It leaves an already-set CheckRedirect alone rather than overriding it.
func withRedirectGuard(client *http.Client) *http.Client {
	if client.CheckRedirect != nil {
		return client
	}
	guarded := *client
	guarded.CheckRedirect = checkRedirectHost
	return &guarded
}

// checkRedirectHost is installed as a download client's CheckRedirect.
// Go's http.Client never calls CheckRedirect for the first request, only
// for each hop after a redirect — so blocking the initial URL is not
// enough on its own; this is what stops a URL that looked fine from
// redirecting the request to a loopback or private address afterward.
func checkRedirectHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return checkHostAllowed(req.Context(), req.URL.Hostname())
}

// blockedAddrDialContext wraps dialer so every connection it makes —
// including one following a redirect, since http.Transport dials again for
// each new host — is refused if it resolves to a blocked address. The
// resolved address is what gets dialed (rather than the hostname a second
// time), so a DNS answer that changes between the check and the connect
// cannot slip an unchecked address past the guard.
func blockedAddrDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip, err := resolveAllowed(ctx, host)
		if err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
}

// checkHostAllowed resolves host and reports an error if any address it
// resolves to is blocked.
func checkHostAllowed(ctx context.Context, host string) error {
	_, err := resolveAllowed(ctx, host)
	return err
}

// resolveAllowed resolves host and returns one of its addresses, refusing
// the whole host if any resolved address is blocked — a hostname that
// resolves to both a public decoy and an internal address must not be let
// through on the strength of the decoy alone.
func resolveAllowed(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedAddr(ip) {
			return nil, fmt.Errorf("%w: %s", errBlockedNetworkAddress, host)
		}
		return ip, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses found for %s", host)
	}
	for _, addr := range addrs {
		if isBlockedAddr(addr.IP) {
			return nil, fmt.Errorf("%w: %s", errBlockedNetworkAddress, host)
		}
	}
	return addrs[0].IP, nil
}

// isBlockedAddr reports whether ip is one pasted markup could use to reach
// this process's own host or its local network rather than a real remote
// image.
func isBlockedAddr(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
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
		// download already enforces maxBytes while streaming, but a data:
		// URI is decoded whole up front with no size check of its own, so
		// this is the only place a too-big PNG/JPEG payload gets caught.
		if int64(len(content)) > maxBytes {
			return nil, fmt.Errorf("image is too big")
		}
		return &Image{Content: content, MimeType: mimeType}, nil
	}

	// Read the declared dimensions before decoding: image.DecodeConfig only
	// parses the header, while image.Decode below allocates a bitmap sized
	// from it. A small, well-compressed file can declare a canvas bounded
	// only by the format's own limits, so the size check below (which runs
	// on the re-encoded PNG) is too late to stop that allocation.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > maxImagePixels {
		return nil, fmt.Errorf("image declares %dx%d pixels, over the %d-pixel limit", cfg.Width, cfg.Height, maxImagePixels)
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
