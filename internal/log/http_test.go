package log

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPRoundTripLogger(t *testing.T) {
	// Create a test server that returns a 500 error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Header", "test-value")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "Internal server error", "code": 500}`))
	}))
	defer server.Close()

	// Create HTTP client with logging
	client := NewHTTPClient()

	// Make a request
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL,
		strings.NewReader(`{"test": "data"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Verify response
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected status code 500, got %d", resp.StatusCode)
	}
}

func TestFormatHeaders(t *testing.T) {
	headers := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{"Bearer secret-token"},
		"X-API-Key":     []string{"api-key-123"},
		"User-Agent":    []string{"test-agent"},
	}

	formatted := formatHeaders(headers)

	// Check that sensitive headers are redacted
	if formatted["Authorization"][0] != "[REDACTED]" {
		t.Error("Authorization header should be redacted")
	}
	if formatted["X-API-Key"][0] != "[REDACTED]" {
		t.Error("X-API-Key header should be redacted")
	}

	// Check that non-sensitive headers are preserved
	if formatted["Content-Type"][0] != "application/json" {
		t.Error("Content-Type header should be preserved")
	}
	if formatted["User-Agent"][0] != "test-agent" {
		t.Error("User-Agent header should be preserved")
	}
}

func TestRedactBody(t *testing.T) {
	in := `{"access_token": "abc", "refresh_token":"r1", "token_type": "bearer", "max_tokens": 5, "note": "keep"}`
	got := redactBody(in)
	for _, leak := range []string{"abc", "r1"} {
		if strings.Contains(got, leak) {
			t.Errorf("secret %q leaked into log: %s", leak, got)
		}
	}
	for _, keep := range []string{`"token_type": "bearer"`, `"max_tokens": 5`, `"note": "keep"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("non-secret %q was mangled: %s", keep, got)
		}
	}
}

// TestHTTPRoundTripLogger_DoesNotBufferStreamingResponse is the regression
// test for --debug collapsing SSE streams: the logger used to drain the
// whole response body inside RoundTrip, so tokens arrived as one late chunk
// after the server closed the stream. Here the server holds the stream open
// until the client has read the first chunk — if the logger drains the body
// up front, client.Do never returns and the test times out.
func TestHTTPRoundTripLogger_DoesNotBufferStreamingResponse(t *testing.T) {
	firstChunkRead := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-firstChunkRead:
		case <-r.Context().Done():
			t.Error("client never read the first chunk")
		}
		_, _ = w.Write([]byte("data: second\n\n"))
	}))
	defer server.Close()

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "first") {
		t.Fatalf("expected to read the first chunk while the stream is open, got %q, err=%v", buf[:n], err)
	}
	close(firstChunkRead)
	rest, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(rest), "second") {
		t.Fatalf("expected the rest of the stream, got %q, err=%v", rest, err)
	}
}
