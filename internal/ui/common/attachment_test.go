package common

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachmentFromPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	content := []byte("hello, world")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	attachment, err := AttachmentFromPath(path)
	require.NoError(t, err)
	require.Equal(t, path, attachment.FilePath)
	require.Equal(t, "note.txt", attachment.FileName)
	require.Equal(t, "text/plain; charset=utf-8", attachment.MimeType)
	require.Equal(t, content, attachment.Content)
}

func TestAttachmentFromPathNonexistent(t *testing.T) {
	t.Parallel()

	_, err := AttachmentFromPath(filepath.Join(t.TempDir(), "missing.txt"))
	require.Error(t, err)
}

func TestAttachmentFromBytes(t *testing.T) {
	t.Parallel()

	content := []byte("paste content")
	attachment := AttachmentFromBytes("paste_1.txt", "paste_1.txt", content)

	require.Equal(t, "paste_1.txt", attachment.FilePath)
	require.Equal(t, "paste_1.txt", attachment.FileName)
	require.Equal(t, "text/plain; charset=utf-8", attachment.MimeType)
	require.Equal(t, content, attachment.Content)
}
