package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// noLastRead is a lastReadTime stub reporting nothing has ever been read,
// for tests that don't exercise the file-tracker branch at all (sessionID
// == "", or a fresh file the tracker has no row for).
func noLastRead(context.Context, string, string) (time.Time, error) {
	return time.Time{}, nil
}

func TestAttachProjectFileUsing_NormalFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o644))

	att, unchanged, err := AttachProjectFileUsing(t.Context(), root, "", "notes.txt", noLastRead)
	require.NoError(t, err)
	require.False(t, unchanged)
	require.Equal(t, "notes.txt", att.FilePath)
	require.Equal(t, "notes.txt", att.FileName)
	require.True(t, att.IsText())
	require.Equal(t, []byte("hello"), att.Content)
}

func TestAttachProjectFileUsing_ResolvesRelativeToRootNotProcessCwd(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o644))

	// The test process's own cwd is wherever `go test` runs from, which is
	// this package's directory - nothing named "notes.txt" lives there.
	// AttachProjectFileUsing must resolve the relative path against root,
	// not os.Getwd(), without this test having to chdir the process (see
	// AGENTS.md on why: other tests may run concurrently in this binary).
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NotEqual(t, root, cwd)
	_, statErr := os.Stat(filepath.Join(cwd, "notes.txt"))
	require.True(t, os.IsNotExist(statErr), "fixture leaked into the process cwd; test is not isolated")

	att, unchanged, err := AttachProjectFileUsing(t.Context(), root, "", "notes.txt", noLastRead)
	require.NoError(t, err)
	require.False(t, unchanged)
	require.Equal(t, []byte("hello"), att.Content)
}

func TestAttachProjectFileUsing_AlreadyReadAndUnchanged(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	lastRead := func(context.Context, string, string) (time.Time, error) {
		return info.ModTime().Add(time.Minute), nil
	}

	att, unchanged, err := AttachProjectFileUsing(t.Context(), root, "sess-1", "notes.txt", lastRead)
	require.NoError(t, err)
	require.True(t, unchanged)
	require.Equal(t, message.Attachment{}, att)
}

func TestAttachProjectFileUsing_ChangedSinceRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	lastRead := func(context.Context, string, string) (time.Time, error) {
		return time.Now().Add(-time.Hour), nil
	}

	att, unchanged, err := AttachProjectFileUsing(t.Context(), root, "sess-1", "notes.txt", lastRead)
	require.NoError(t, err)
	require.False(t, unchanged)
	require.Equal(t, []byte("hello"), att.Content)
}

func TestAttachProjectFileUsing_Missing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, unchanged, err := AttachProjectFileUsing(t.Context(), root, "", "nope.txt", noLastRead)
	require.ErrorIs(t, err, ErrAttachFileMissing)
	require.False(t, unchanged)
}

func TestAttachProjectFileUsing_Directory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "subdir"), 0o755))

	_, unchanged, err := AttachProjectFileUsing(t.Context(), root, "", "subdir", noLastRead)
	require.ErrorIs(t, err, ErrAttachIsDirectory)
	require.False(t, unchanged)
}

func TestAttachProjectFileUsing_TooBig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "big.bin")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(message.MaxAttachmentSize+1))
	require.NoError(t, f.Close())

	_, unchanged, err := AttachProjectFileUsing(t.Context(), root, "", "big.bin", noLastRead)
	require.ErrorIs(t, err, ErrAttachTooBig)
	require.False(t, unchanged)
}

func TestAttachProjectFileUsing_UnsupportedType(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "data.bin")
	// Bytes that http.DetectContentType sniffs as application/octet-stream:
	// arbitrary non-text, non-image binary content.
	require.NoError(t, os.WriteFile(path, []byte{0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE}, 0o644))

	_, unchanged, err := AttachProjectFileUsing(t.Context(), root, "", "data.bin", noLastRead)
	require.ErrorIs(t, err, ErrAttachUnsupportedType)
	require.False(t, unchanged)
}

func TestAttachProjectFileUsing_LastReadTimeErrorIsLoggedNotFatal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o644))

	lastRead := func(context.Context, string, string) (time.Time, error) {
		return time.Time{}, errors.New("tracker unavailable")
	}

	att, unchanged, err := AttachProjectFileUsing(t.Context(), root, "sess-1", "notes.txt", lastRead)
	require.NoError(t, err)
	require.False(t, unchanged)
	require.Equal(t, []byte("hello"), att.Content)
}
