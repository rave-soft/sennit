package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// TestDropTrailingPartialRune pins the size cap against valid content. A
// byte-limited read cuts at an offset, which lands mid-rune on any page
// whose multi-byte character straddles it — and the validity check that
// followed then rejected a perfectly good page with "not valid UTF-8"
// purely for being longer than the cap.
func TestDropTrailingPartialRune(t *testing.T) {
	t.Parallel()

	// "я" is two bytes; cut between them, exactly as io.LimitReader would.
	full := []byte(strings.Repeat("a", 10) + "я")
	cut := full[:len(full)-1]
	require.False(t, utf8.Valid(cut), "the setup must actually produce a split rune")

	got := dropTrailingPartialRune(cut)
	require.True(t, utf8.Valid(got), "a trailing partial rune must be dropped")
	require.Equal(t, strings.Repeat("a", 10), string(got))
}

// TestDropTrailingPartialRuneLeavesValidContentAlone keeps the trim from
// eating anything it should not, including a genuine trailing U+FFFD.
func TestDropTrailingPartialRuneLeavesValidContentAlone(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "plain ascii", "многобайтовый текст", "ends with �"} {
		require.Equal(t, s, string(dropTrailingPartialRune([]byte(s))), "input %q", s)
	}
}

// TestDropTrailingPartialRuneKeepsInvalidContentInvalid keeps the check
// this feeds meaningful: content that is malformed in its own right still
// fails validation.
func TestDropTrailingPartialRuneKeepsInvalidContentInvalid(t *testing.T) {
	t.Parallel()

	bad := []byte{'a', 0xff, 'b'}
	require.False(t, utf8.Valid(dropTrailingPartialRune(bad)))
}

func TestFetchURLAndConvertRejectsResponseOverBodyLimit(t *testing.T) {
	t.Parallel()

	const maxSize = 5 * 1024 * 1024
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("a", maxSize+1)))
	}))
	t.Cleanup(server.Close)

	content, err := FetchURLAndConvert(t.Context(), server.Client(), server.URL)
	require.Error(t, err)
	require.Empty(t, content)
	require.Contains(t, err.Error(), "response body exceeds")
}

func TestFetchURLAndConvertAcceptsResponseAtBodyLimit(t *testing.T) {
	t.Parallel()

	const maxSize = 5 * 1024 * 1024
	want := strings.Repeat("a", maxSize)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(want))
	}))
	t.Cleanup(server.Close)

	content, err := FetchURLAndConvert(t.Context(), server.Client(), server.URL)
	require.NoError(t, err)
	require.Equal(t, want, content)
}
