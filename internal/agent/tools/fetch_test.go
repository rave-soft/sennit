package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
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
