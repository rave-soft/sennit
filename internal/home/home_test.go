package home

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDir(t *testing.T) {
	require.NotEmpty(t, Dir())
}

func TestShort(t *testing.T) {
	d := filepath.Join(Dir(), "documents", "file.txt")
	require.Equal(t, filepath.FromSlash("~/documents/file.txt"), Short(d))
	ad := filepath.FromSlash("/absolute/path/file.txt")
	require.Equal(t, ad, Short(ad))
}

// TestShort_SiblingWithSharedPrefix verifies that Short only strips homedir
// when it is a genuine path ancestor (followed by a separator, or an exact
// match), not merely a string prefix. Without the separator check, home
// "/home/bob" would match unrelated sibling "/home/bobby" too, rendering it
// as "~by" instead of leaving it alone.
func TestShort_SiblingWithSharedPrefix(t *testing.T) {
	orig := homedir
	homedir = filepath.FromSlash("/home/bob")
	defer func() { homedir = orig }()

	sibling := filepath.FromSlash("/home/bobby/file.txt")
	require.Equal(t, sibling, Short(sibling))

	inside := filepath.FromSlash("/home/bob/file.txt")
	require.Equal(t, filepath.FromSlash("~/file.txt"), Short(inside))

	exact := filepath.FromSlash("/home/bob")
	require.Equal(t, "~", Short(exact))
}

func TestLong(t *testing.T) {
	d := filepath.FromSlash("~/documents/file.txt")
	require.Equal(t, filepath.Join(Dir(), "documents", "file.txt"), Long(d))
	ad := filepath.FromSlash("/absolute/path/file.txt")
	require.Equal(t, ad, Long(ad))
}
