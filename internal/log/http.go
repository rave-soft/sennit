package log

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// NewHTTPClient creates an HTTP client with debug logging enabled when debug mode is on.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &HTTPRoundTripLogger{
			Transport: http.DefaultTransport,
		},
	}
}

// HTTPRoundTripLogger is an http.RoundTripper that logs requests and responses.
type HTTPRoundTripLogger struct {
	Transport http.RoundTripper
}

// maxLoggedBody caps how much of a body is kept for logging, so a large
// download or a long stream does not pin unbounded memory just because
// debug logging is on.
const maxLoggedBody = 64 * 1024

// RoundTrip implements http.RoundTripper interface with logging.
func (h *HTTPRoundTripLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	var err error
	var save io.ReadCloser
	save, req.Body, err = drainBody(req.Body)
	if err != nil {
		slog.Error(
			"HTTP request failed",
			"method", req.Method,
			"url", req.URL,
			"error", err,
		)
		return nil, err
	}

	if slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		slog.Debug(
			"HTTP Request",
			"method", req.Method,
			"url", req.URL,
			"body", bodyToString(save),
		)
	}

	start := time.Now()
	resp, err := h.Transport.RoundTrip(req)
	duration := time.Since(start)
	if err != nil {
		slog.Error(
			"HTTP request failed",
			"method", req.Method,
			"url", req.URL,
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
		return resp, err
	}

	if !slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		return resp, nil
	}

	slog.Debug(
		"HTTP Response",
		"status_code", resp.StatusCode,
		"status", resp.Status,
		"headers", formatHeaders(resp.Header),
		"content_length", resp.ContentLength,
		"duration_ms", duration.Milliseconds(),
	)

	// The body is logged when the caller finishes reading it, not here:
	// draining it up front would stall a streaming (SSE) response until the
	// server closes it, turning a token stream into one late chunk.
	method, url, status := req.Method, req.URL.String(), resp.StatusCode
	if resp.Body != nil && resp.Body != http.NoBody {
		resp.Body = &loggedBody{
			rc: resp.Body,
			logFn: func(body string, truncated bool) {
				slog.Debug(
					"HTTP Response body",
					"method", method,
					"url", url,
					"status_code", status,
					"body", body,
					"truncated", truncated,
				)
			},
		}
	}
	return resp, nil
}

// loggedBody tees what the caller reads into a capped buffer and emits one
// log record when the stream ends (EOF or Close), whichever comes first.
type loggedBody struct {
	rc    io.ReadCloser
	buf   bytes.Buffer
	seen  int64
	once  sync.Once
	logFn func(body string, truncated bool)
}

func (l *loggedBody) Read(p []byte) (int, error) {
	n, err := l.rc.Read(p)
	if n > 0 {
		l.seen += int64(n)
		if room := maxLoggedBody - l.buf.Len(); room > 0 {
			l.buf.Write(p[:min(n, room)])
		}
	}
	if err == io.EOF {
		l.emit()
	}
	return n, err
}

func (l *loggedBody) Close() error {
	l.emit()
	return l.rc.Close()
}

func (l *loggedBody) emit() {
	l.once.Do(func() {
		l.logFn(redactBody(indentJSON(l.buf.Bytes())), l.seen > int64(l.buf.Len()))
	})
}

func bodyToString(body io.ReadCloser) string {
	if body == nil {
		return ""
	}
	src, err := io.ReadAll(io.LimitReader(body, maxLoggedBody))
	if err != nil {
		slog.Error("Failed to read body", "error", err)
		return ""
	}
	return redactBody(indentJSON(src))
}

func indentJSON(src []byte) string {
	var b bytes.Buffer
	if json.Indent(&b, bytes.TrimSpace(src), "", "  ") != nil {
		// not json probably
		return string(src)
	}
	return b.String()
}

// sensitiveBodyFields matches JSON string fields whose value is a
// credential, wherever they appear in a logged body. Headers are already
// redacted by formatHeaders; bodies carry the same secrets on OAuth
// token-exchange and API-key endpoints, so they get the same treatment.
var sensitiveBodyFields = regexp.MustCompile(
	`(?i)("(?:access_token|refresh_token|id_token|token|api_key|apikey|client_secret|secret|password|authorization)"\s*:\s*)"(?:[^"\\]|\\.)*"`,
)

func redactBody(s string) string {
	return sensitiveBodyFields.ReplaceAllString(s, `$1"[REDACTED]"`)
}

// formatHeaders formats HTTP headers for logging, filtering out sensitive information.
func formatHeaders(headers http.Header) map[string][]string {
	filtered := make(map[string][]string)
	for key, values := range headers {
		lowerKey := strings.ToLower(key)
		// Filter out sensitive headers
		if strings.Contains(lowerKey, "authorization") ||
			strings.Contains(lowerKey, "api-key") ||
			strings.Contains(lowerKey, "token") ||
			strings.Contains(lowerKey, "secret") {
			filtered[key] = []string{"[REDACTED]"}
		} else {
			filtered[key] = values
		}
	}
	return filtered
}

func drainBody(b io.ReadCloser) (r1, r2 io.ReadCloser, err error) {
	if b == nil || b == http.NoBody {
		return http.NoBody, http.NoBody, nil
	}
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(b); err != nil {
		return nil, b, err
	}
	if err = b.Close(); err != nil {
		return nil, b, err
	}
	return io.NopCloser(&buf), io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}
