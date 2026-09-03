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

// TestLong_OnlyBareTildeOrTildeSlashExpand pins the fix for G23: Long used
// to check strings.HasPrefix(p, "~") rather than "~/", so "~alice/bin"
// under home "/home/bob" turned into "/home/bobalice/bin" — a path
// nobody asked for. Only a bare "~" and "~/..." name this user's own
// home; "~user/..." (another user's home) has no way to be resolved by
// this package and must be left alone.
func TestLong_OnlyBareTildeOrTildeSlashExpand(t *testing.T) {
	orig := homedir
	homedir = filepath.FromSlash("/home/bob")
	defer func() { homedir = orig }()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde", "~", "/home/bob"},
		{"tilde slash", "~/x", "/home/bob/x"},
		{"other user's home with path", "~alice/bin/server", "~alice/bin/server"},
		{"other user's home, bare", "~alice", "~alice"},
		{"empty string", "", ""},
		{"ordinary path", "/etc/passwd", "/etc/passwd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, filepath.FromSlash(c.want), Long(filepath.FromSlash(c.in)))
		})
	}
}
