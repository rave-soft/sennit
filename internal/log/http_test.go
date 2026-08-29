package log

import (
	"fmt"
	"io"
	"log/slog"
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
	client := newHTTPClient()

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

// TestIsSensitiveName covers the widened redaction rule: any header or JSON
// field name whose last "word" (split on separators and camelCase
// boundaries) is a credential-shaped term gets redacted, while a word that
// merely contains one of those terms as a substring does not.
func TestIsSensitiveName(t *testing.T) {
	sensitive := []string{
		"Authorization",
		"authorization",
		"Cookie",
		"Set-Cookie",
		"set_cookie",
		"key",
		"X-Api-Key",
		"x-api-key",
		"apiKey",
		"apikey",
		"credential",
		"credentials",
		"private_key",
		"privateKey",
		"privatekey",
		"client_secret",
		"secret",
		"password",
		"access_token",
		"refresh_token",
		"id_token",
		"token",
	}
	for _, name := range sensitive {
		if !isSensitiveName(name) {
			t.Errorf("isSensitiveName(%q) = false, want true", name)
		}
	}

	notSensitive := []string{
		"keyboard",
		"monkey",
		"Content-Type",
		"User-Agent",
		"token_type",
		"max_tokens",
		"note",
	}
	for _, name := range notSensitive {
		if isSensitiveName(name) {
			t.Errorf("isSensitiveName(%q) = true, want false", name)
		}
	}
}

// TestRedactBody_WidenedFields is the regression test for issue 1: bodies
// carrying a cookie, a bare "key", a credential, or a private_key used to be
// logged verbatim because the deny-list only covered a handful of exact
// field names.
func TestRedactBody_WidenedFields(t *testing.T) {
	in := `{"cookie": "s3ss10n", "key": "abc123", "credential": "cred1", ` +
		`"private_key": "pk1", "keyboard": "qwerty", "monkey": "banana"}`
	got := redactBody(in)
	for _, leak := range []string{"s3ss10n", "abc123", "cred1", "pk1"} {
		if strings.Contains(got, leak) {
			t.Errorf("secret %q leaked into log: %s", leak, got)
		}
	}
	for _, keep := range []string{"qwerty", "banana"} {
		if !strings.Contains(got, keep) {
			t.Errorf("non-secret %q was mangled: %s", keep, got)
		}
	}
}

// TestFormatHeaders_WidenedFields is the regression test for issue 1: a
// cookie-based auth header or a raw "key"/"credential" header used to pass
// through formatHeaders unredacted.
func TestFormatHeaders_WidenedFields(t *testing.T) {
	headers := http.Header{
		"Cookie":       []string{"session=abc123"},
		"Set-Cookie":   []string{"session=abc123; Path=/"},
		"Key":          []string{"raw-key-value"},
		"Credential":   []string{"cred-value"},
		"X-Keyboard":   []string{"qwerty"},
		"X-Monkey-Bar": []string{"banana"},
	}

	formatted := formatHeaders(headers)

	for _, redacted := range []string{"Cookie", "Set-Cookie", "Key", "Credential"} {
		if formatted[redacted][0] != "[REDACTED]" {
			t.Errorf("%s header should be redacted, got %v", redacted, formatted[redacted])
		}
	}
	if formatted["X-Keyboard"][0] != "qwerty" {
		t.Error("X-Keyboard header should be preserved")
	}
	if formatted["X-Monkey-Bar"][0] != "banana" {
		t.Error("X-Monkey-Bar header should be preserved")
	}
}

// TestHTTPRoundTripLogger_BodyIdenticalWithAndWithoutDebug is the regression
// test for issue 2: the request body must reach the server byte-for-byte
// the same whether debug logging is on or off, since the fix skips buffering
// the body entirely when the level would discard it.
func TestHTTPRoundTripLogger_BodyIdenticalWithAndWithoutDebug(t *testing.T) {
	const payload = `{"hello": "world", "token": "s3cr3t"}`

	for _, debugOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%v", debugOn), func(t *testing.T) {
			var received string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				received = string(body)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			level := slog.LevelInfo
			if debugOn {
				level = slog.LevelDebug
			}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: level})))
			defer slog.SetDefault(prev)

			client := newHTTPClient()
			req, err := http.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				server.URL,
				strings.NewReader(payload),
			)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
			if received != payload {
				t.Errorf("server received %q, want %q", received, payload)
			}
		})
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

	client := newHTTPClient()
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
