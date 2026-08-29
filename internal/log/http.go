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
	"unicode"
)

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
	// Only drain (and thus buffer) the request body when debug logging would
	// actually use it. A drained-but-discarded body still costs memory equal
	// to its full size, so a large upload should never pay for that when the
	// level would drop the log record anyway.
	if slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		save, body, err := drainBody(req.Body)
		if err != nil {
			slog.Error(
				"HTTP request failed",
				"method", req.Method,
				"url", req.URL,
				"error", err,
			)
			return nil, err
		}
		req.Body = body

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

// sensitiveNameWords are the last "word" of a header or JSON field name that
// mark it as carrying a credential. Matching is on the last word rather than
// any substring so that a compound name like "token_type" or "max_tokens"
// (metadata *about* a credential, not the credential itself) is left alone,
// while "access_token", "x-api-key", "apiKey", "private_key", "Set-Cookie"
// and "client_secret" are all caught without enumerating every spelling.
// "apikey" and "privatekey" are listed whole because, unlike the others,
// they're commonly written as one word with no separator to split on.
var sensitiveNameWords = map[string]bool{
	"authorization": true,
	"token":         true,
	"secret":        true,
	"password":      true,
	"cookie":        true,
	"credential":    true,
	"credentials":   true,
	"key":           true,
	"apikey":        true,
	"privatekey":    true,
}

// isSensitiveName reports whether a header or JSON field name should be
// redacted before logging. It splits the name on non-alphanumeric
// separators and camelCase boundaries, then checks the last resulting word
// against sensitiveNameWords — e.g. "X-Api-Key" and "client_secret" both
// end in a sensitive word, but "keyboard" and "monkey" (one word, not
// "key") and "token_type"/"max_tokens" (last word "type"/"tokens", not
// "token") do not.
func isSensitiveName(name string) bool {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	words := strings.FieldsFunc(strings.ToLower(b.String()), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(words) == 0 {
		return false
	}
	return sensitiveNameWords[words[len(words)-1]]
}

// jsonStringField matches a JSON object field with a string value, so
// redactBody can inspect each field name in turn.
var jsonStringField = regexp.MustCompile(`"([A-Za-z0-9_-]+)"(\s*:\s*)"(?:[^"\\]|\\.)*"`)

// redactBody replaces the value of every sensitive-looking string field in
// a logged JSON body. It shares isSensitiveName with formatHeaders so a
// credential is caught the same way whether it travels in a header or the
// body.
func redactBody(s string) string {
	return jsonStringField.ReplaceAllStringFunc(s, func(m string) string {
		sub := jsonStringField.FindStringSubmatch(m)
		name, sep := sub[1], sub[2]
		if !isSensitiveName(name) {
			return m
		}
		return `"` + name + `"` + sep + `"[REDACTED]"`
	})
}

// formatHeaders formats HTTP headers for logging, filtering out sensitive information.
func formatHeaders(headers http.Header) map[string][]string {
	filtered := make(map[string][]string)
	for key, values := range headers {
		if isSensitiveName(key) {
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
