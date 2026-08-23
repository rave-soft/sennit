package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
