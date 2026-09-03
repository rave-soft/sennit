package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

// erroringRoundTripper fails every request instantly, standing in for a DNS
// failure or a refused connection without a real network dependency.
type erroringRoundTripper struct{ err error }

func (rt erroringRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, rt.err
}

// TestFetchToolNetworkFailureIsTextResponseNotError pins an error-vs-response
// fix: a request that fails to reach the model-supplied URL (DNS failure,
// connection refused, timeout) is information about that URL, not about this
// process, so it comes back as a normal (IsError) tool result the model can
// react to — e.g. by trying a different URL — instead of a Go error that
// aborts the whole tool-call batch. This matches web_fetch's handling of the
// same failure (see web_fetch.go).
func TestFetchToolRejectsSchemaDriftValues(t *testing.T) {
	tool := NewFetchTool(&stubPermissionService{granted: true}, t.TempDir(), &http.Client{Transport: erroringRoundTripper{err: errors.New("unused")}})
	for _, params := range []FetchParams{{URL: "https://example.invalid", Format: "TEXT"}, {URL: "https://example.invalid", Format: "text", Timeout: -1}, {URL: "https://example.invalid", Format: "text", Timeout: 121}} {
		input, err := json.Marshal(params)
		require.NoError(t, err)
		resp, err := tool.Run(context.WithValue(context.Background(), SessionIDContextKey, "test-session"), fantasy.ToolCall{Input: string(input)})
		require.NoError(t, err)
		require.True(t, resp.IsError)
	}
}

// requestCountingPermissionService wraps stubPermissionService to count how
// many times Request is called, so a test can tell whether the permission
// dialog was ever reached.
type requestCountingPermissionService struct {
	*stubPermissionService
	requests int
}

func (s *requestCountingPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	s.requests++
	return s.stubPermissionService.Request(ctx, req)
}

// TestFetchTool_InvalidTimeoutIsRejectedBeforePermissionDialog is the
// regression test for G25: the timeout bound used to be checked only after
// requirePermission returned, so a model (or a user in the dialog) would
// approve a fetch that then failed on a check that could have run first —
// matching download.go, which already validates timeout before asking.
func TestFetchTool_InvalidTimeoutIsRejectedBeforePermissionDialog(t *testing.T) {
	t.Parallel()

	perms := &requestCountingPermissionService{stubPermissionService: &stubPermissionService{granted: true}}
	tool := NewFetchTool(perms, t.TempDir(), &http.Client{Transport: erroringRoundTripper{err: errors.New("unused")}})

	resp, err := tool.Run(context.WithValue(context.Background(), SessionIDContextKey, "test-session"), fantasy.ToolCall{
		Input: `{"url":"https://example.invalid","format":"text","timeout":121}`,
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "timeout must be between")
	require.Zero(t, perms.requests, "an invalid timeout must be rejected before the permission dialog is shown")
}

func TestFetchToolNetworkFailureIsTextResponseNotError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("dial tcp: lookup example.invalid: no such host")
	client := &http.Client{Transport: erroringRoundTripper{err: wantErr}}
	perms := &stubPermissionService{granted: true}
	tool := NewFetchTool(perms, t.TempDir(), client)

	input, err := json.Marshal(FetchParams{URL: "https://example.invalid", Format: "text"})
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Failed to fetch URL")
	require.Contains(t, resp.Content, wantErr.Error())
}

// TestFetchTool_DefaultClientDoesNotCapBelowCallerTimeout pins DEFECT 1:
// NewFetchTool used to build its default client with a fixed 30s
// http.Client.Timeout in addition to the per-call context timeout derived
// from the "timeout" parameter. http.Client.Timeout bounds the whole
// request regardless of context, so a caller-supplied timeout longer than
// whatever that fixed value was got silently capped at it. Reproduced here
// at a scale that doesn't require waiting on the real 30s/120s figures: a
// tool built with an explicit low-Timeout client (standing in for "the old
// capped default") still gets cut off even though it was asked for a
// longer per-call timeout, while a tool built with client: nil - the
// actual production default-construction path - honors that same longer
// timeout, proving nothing in NewFetchTool's own default client caps it.
func TestFetchTool_DefaultClientDoesNotCapBelowCallerTimeout(t *testing.T) {
	t.Parallel()

	const slowServerDelay = 300 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(slowServerDelay)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	perms := &stubPermissionService{granted: true}
	// requested timeout (2s) comfortably exceeds both the slow server's
	// delay and the stand-in "old cap" (100ms) below, so a failure here can
	// only come from that fixed client-level cap, not from the request
	// simply running long.
	params := FetchParams{URL: server.URL, Format: "text", Timeout: 2}
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")

	cappedClient := &http.Client{Timeout: 100 * time.Millisecond}
	capped := NewFetchTool(perms, t.TempDir(), cappedClient)
	resp, err := capped.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError, "a client-level Timeout below the caller's requested timeout must still cut the request short")

	uncapped := NewFetchTool(perms, t.TempDir(), nil)
	resp, err = uncapped.Run(ctx, fantasy.ToolCall{ID: "call-2", Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError, "NewFetchTool's default client (client: nil) must not impose a fixed cap below the caller's timeout")
	require.Equal(t, "ok", resp.Content)
}

// TestFetchTool_DirectLoopbackFetchSucceeds is the regression guard for the
// legitimate case a redirect guard must not break: "fetch my local dev
// server on localhost" is a normal, user-approved request, and the initial
// URL is never subject to the blocked-address check — only a later redirect
// hop is (see checkFetchRedirect).
func TestFetchTool_DirectLoopbackFetchSucceeds(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("dev server ok"))
	}))
	t.Cleanup(server.Close)

	perms := &stubPermissionService{granted: true}
	tool := NewFetchTool(perms, t.TempDir(), nil)

	input, err := json.Marshal(FetchParams{URL: server.URL, Format: "text"})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "dev server ok", resp.Content)
}

// TestFetchTool_RefusesRedirectFromApprovedHostToLinkLocalAddress pins the
// actual fix: the user approves the URL they see (the test server's
// loopback address here, standing in for any host), not wherever the server
// then redirects to. 169.254.169.254 is the classic cloud-metadata SSRF
// target — a link-local address the redirect leaves the approved host for,
// so it must be refused before the client ever dials it.
func TestFetchTool_RefusesRedirectFromApprovedHostToLinkLocalAddress(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	perms := &stubPermissionService{granted: true}
	tool := NewFetchTool(perms, t.TempDir(), nil)

	input, err := json.Marshal(FetchParams{URL: server.URL, Format: "text"})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "refusing to follow a redirect")
}

// staticRoundTripper serves canned responses keyed by request URL, standing
// in for a redirect chain across hosts without any real network dial — used
// so the cross-host case below cannot depend on outbound network access or
// real DNS.
type staticRoundTripper struct {
	responses map[string]*http.Response
}

func (rt staticRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, ok := rt.responses[req.URL.String()]
	if !ok {
		return nil, fmt.Errorf("unstubbed request: %s", req.URL)
	}
	resp.Request = req
	return resp, nil
}

// TestFetchTool_OrdinaryCrossHostRedirectStillWorks confirms the guard is
// about address, not host identity: a redirect that leaves the approved
// host but lands on an ordinary public address must still be followed, or
// every cross-host redirect (a bare domain to its "www" host, a link
// shortener, a CDN) would break.
func TestFetchTool_OrdinaryCrossHostRedirectStillWorks(t *testing.T) {
	t.Parallel()

	const startURL = "https://198.51.100.1/start"
	const finalURL = "https://198.51.100.2/final"
	client := &http.Client{
		Transport: staticRoundTripper{responses: map[string]*http.Response{
			startURL: {
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{finalURL}},
				Body:       http.NoBody,
			},
			finalURL: {
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("final content")),
			},
		}},
		CheckRedirect: checkFetchRedirect,
	}

	perms := &stubPermissionService{granted: true}
	tool := NewFetchTool(perms, t.TempDir(), client)

	input, err := json.Marshal(FetchParams{URL: startURL, Format: "text"})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "final content", resp.Content)
}

// TestTruncateToRuneBoundary pins the fix for content[:MaxFetchSize]
// cutting a multi-byte UTF-8 rune in half: format=html/markdown can wrap
// an already MaxFetchSize-sized body in extra markup, pushing content past
// the limit at a byte offset that need not land on a rune boundary. A raw
// byte-index slice can then leave an invalid trailing fragment; this must
// back off to the nearest earlier rune boundary instead.
func TestTruncateToRuneBoundary(t *testing.T) {
	t.Parallel()

	t.Run("a rune straddling the cut point is dropped whole, not split", func(t *testing.T) {
		t.Parallel()
		prefix := strings.Repeat("a", MaxFetchSize-1)
		// "世" is 3 bytes; its first byte lands at index MaxFetchSize-1, so
		// a plain byte-index cut at MaxFetchSize would keep only that
		// first byte - an invalid, truncated rune.
		s := prefix + "世" + "trailing content"
		got := truncateToRuneBoundary(s, MaxFetchSize)
		require.True(t, utf8.ValidString(got), "must never cut a rune in half")
		require.Equal(t, prefix, got, "the straddling rune must be dropped whole")
	})

	t.Run("a cut point already on a rune boundary is unaffected", func(t *testing.T) {
		t.Parallel()
		want := strings.Repeat("a", MaxFetchSize)
		got := truncateToRuneBoundary(want+"trailing", MaxFetchSize)
		require.Equal(t, want, got)
	})

	t.Run("n at or beyond the string's length is a no-op", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "short", truncateToRuneBoundary("short", 100))
	})
}

// TestFetchTool_ReportsTruncationEvenAfterConversionShrinksContent is the
// regression test for truncation being reported only when the CONVERTED
// output is still >= MaxFetchSize: a raw HTML body over the cap converts to
// markdown far smaller than MaxFetchSize (a page dense in markup collapses
// to compact text), so checking len(content) after conversion missed the
// truncation entirely and presented a cut-off page as complete. The fix
// reads MaxFetchSize+1 raw bytes and reports truncation off that raw read,
// independent of how much the conversion step shrinks it afterward.
func TestFetchTool_ReportsTruncationEvenAfterConversionShrinksContent(t *testing.T) {
	t.Parallel()

	// A page whose raw HTML exceeds MaxFetchSize but whose markdown
	// conversion is tiny: a long run of empty, attribute-heavy divs
	// (markup, not text) followed by one real paragraph.
	var raw strings.Builder
	raw.WriteString("<html><body>")
	for raw.Len() < MaxFetchSize+1000 {
		raw.WriteString(`<div class="noise" data-x="1" data-y="2"></div>`)
	}
	raw.WriteString("<p>hello</p></body></html>")
	require.Greater(t, raw.Len(), MaxFetchSize, "test setup: raw body must exceed the fetch cap")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(raw.String()))
	}))
	t.Cleanup(server.Close)

	tool := NewFetchTool(&stubPermissionService{granted: true}, t.TempDir(), server.Client())
	input, err := json.Marshal(FetchParams{URL: server.URL, Format: "markdown"})
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	require.Less(t, len(resp.Content), MaxFetchSize,
		"test setup: the converted markdown must be far smaller than the raw cap, or this test doesn't exercise the bug")
	require.Contains(t, resp.Content, "[Content truncated to",
		"a page cut short by the fetch cap must still say so, even after conversion shrinks it well under the cap")
}
