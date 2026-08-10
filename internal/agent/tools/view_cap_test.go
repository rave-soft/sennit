package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A file bigger than MaxViewSize must be returned truncated at the cap
// with continuation guidance, not fail the whole read.
func TestReadTextFile_TruncatesAtSizeCapInsteadOfFailing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	line := strings.Repeat("x", 1000)
	var b strings.Builder
	for range 300 { // ~300KB > MaxViewSize
		b.WriteString(line)
		b.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))

	content, hasMore, err := readTextFile(path, 0, 100000, MaxViewSize)
	require.NoError(t, err, "size cap must truncate, not error")
	require.True(t, hasMore, "truncated read must advertise continuation")
	require.NotEmpty(t, content)
	require.LessOrEqual(t, len(content), MaxViewSize)
	got := len(strings.Split(content, "\n"))
	require.Greater(t, got, 100, "should return a substantial prefix")
}
