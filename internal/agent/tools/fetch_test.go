package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

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
