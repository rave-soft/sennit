package chat

import (
	"strings"
	"testing"
	"unicode/utf8"

	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/stretchr/testify/require"
)

// TestQuestionSummary_CyrillicNotByteSliced pins that questionSummary
// truncates by display width (ansi.Truncate), not by raw byte offset.
// Cyrillic characters are 2 bytes each in UTF-8, so a byte-offset slice
// like text[:59] lands in the middle of a rune roughly half the time,
// producing an invalid UTF-8 tail that renders as "�" in the terminal.
func TestQuestionSummary_CyrillicNotByteSliced(t *testing.T) {
	t.Parallel()

	// 40 Cyrillic runes (80 bytes) — comfortably over both the 60-byte
	// and 60-rune thresholds, so any byte-offset slice at an odd byte
	// count splits a rune.
	question := strings.Repeat("П", 40)
	params := tools.QuestionParams{Questions: []tools.QuestionItem{{Question: question}}}

	out := questionSummary(params)

	require.True(t, utf8.ValidString(out), "byte-sliced Cyrillic text must not produce invalid UTF-8: %q", out)
	require.NotContains(t, out, "�")
}

// TestQuestionSummary_MultiQuestionCyrillicNotByteSliced covers the
// "+N more" branch (the 40-byte/width cap), which had the same byte-slice
// bug as the single-question branch.
func TestQuestionSummary_MultiQuestionCyrillicNotByteSliced(t *testing.T) {
	t.Parallel()

	question := strings.Repeat("Ж", 30)
	params := tools.QuestionParams{Questions: []tools.QuestionItem{
		{Question: question},
		{Question: "second question"},
	}}

	out := questionSummary(params)

	require.True(t, utf8.ValidString(out), "byte-sliced Cyrillic text must not produce invalid UTF-8: %q", out)
	require.NotContains(t, out, "�")
	require.Contains(t, out, "(+1 more)")
}
