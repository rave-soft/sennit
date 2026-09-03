package dialog

import (
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	fimage "github.com/rave-soft/sennit/internal/ui/image"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func testPicker(t *testing.T) *FilePicker {
	t.Helper()
	styles := styles.SennitDark()
	picker, _ := NewFilePicker(&common.Common{Styles: &styles})
	picker.imgPrevWidth = 2
	picker.imgPrevHeight = 1
	return picker
}

func png() []byte {
	return []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0x00, 0x00, 0x03, 0x01, 0x01, 0x00, 0xc9, 0xfe, 0x92, 0xef, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}
}

func prepare(t *testing.T, picker *FilePicker, path string) fimage.PreviewPreparedMsg {
	t.Helper()
	msg := picker.preparePreviewCmd(picker.instance, path, picker.generation, picker.previewKey(path), picker.cacheSnapshot())()
	prepared, ok := msg.(fimage.PreviewPreparedMsg)
	require.True(t, ok)
	return prepared
}

func TestPreviewKittyOutputIsAppliedAfterAcceptedResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	require.NoError(t, os.WriteFile(path, png(), 0o600))
	picker := testPicker(t)
	picker.imgEnc = fimage.EncodingKitty
	picker.selectedPath = path
	picker.generation = 1
	result := prepare(t, picker, path)
	require.NoError(t, result.Err)
	require.NotEmpty(t, result.Output)
	action := picker.HandleMsg(result).(ActionCmd)
	require.NotEmpty(t, picker.preview)
	raw, ok := action.Cmd().(tea.RawMsg)
	require.True(t, ok)
	require.Equal(t, result.Output, raw.Msg)
}

// hugePNGHeader builds only the PNG signature and a valid IHDR chunk
// declaring a huge canvas, with no pixel data at all. image.DecodeConfig
// only needs IHDR to report dimensions, so this is enough to exercise the
// pixel-limit rejection without ever allocating (or even producing) the
// gigabytes of image data the declared size implies.
func hugePNGHeader(width, height uint32) []byte {
	sig := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	data := []byte{
		byte(width >> 24), byte(width >> 16), byte(width >> 8), byte(width),
		byte(height >> 24), byte(height >> 16), byte(height >> 8), byte(height),
		8, 2, 0, 0, 0, // 8-bit truecolor, no interlace.
	}
	length := len(data)
	chunk := []byte{byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
	chunk = append(chunk, "IHDR"...)
	chunk = append(chunk, data...)
	crc := crc32.ChecksumIEEE(chunk[4:])
	chunk = append(chunk, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return append(sig, chunk...)
}

func TestPreviewRejectsExcessivePixelDimensionsWithoutDecoding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.png")
	require.NoError(t, os.WriteFile(path, hugePNGHeader(20000, 20000), 0o600))
	picker := testPicker(t)
	picker.selectedPath = path
	picker.generation = 1
	result := prepare(t, picker, path)
	require.ErrorContains(t, result.Err, "pixel limit")
	require.Empty(t, result.Rendered)
}

func TestPreviewBlocksIsPrepared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	require.NoError(t, os.WriteFile(path, png(), 0o600))
	picker := testPicker(t)
	picker.selectedPath = path
	picker.generation = 1
	result := prepare(t, picker, path)
	require.NoError(t, result.Err)
	require.NotEmpty(t, result.Rendered)
	require.Empty(t, result.Output)
}

func TestStaleAndInvalidSelectionsClearPreview(t *testing.T) {
	picker := testPicker(t)
	picker.selectedPath = "current.png"
	picker.generation = 2
	picker.preview = "current"
	picker.HandleMsg(fimage.PreviewPreparedMsg{Key: fimage.PreviewKey{Path: "old.png"}, Generation: 1, Rendered: "old"})
	require.Equal(t, "current", picker.preview)
	picker.selectedPath = ""
	picker.invalidatePreview()
	require.Empty(t, picker.preview)
	picker.selectedPath = "notes.txt"
	picker.invalidatePreview()
	require.Empty(t, picker.preview)
}

func TestFilePickerUpdateResizesAndRepreparesPreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	require.NoError(t, os.WriteFile(path, png(), 0o600))
	picker := testPicker(t)
	picker.selectedPath = path
	overlay := NewOverlay(picker)

	first := overlay.Update(FilePickerUpdateMsg{WindowSize: &tea.WindowSizeMsg{Width: 100, Height: 30}}).(ActionCmd).Cmd().(fimage.PreviewPreparedMsg)
	require.NoError(t, first.Err)
	require.Positive(t, first.Key.Columns)
	require.Positive(t, first.Key.Rows)
	overlay.Update(first)

	second := overlay.Update(FilePickerUpdateMsg{WindowSize: &tea.WindowSizeMsg{Width: 60, Height: 20}}).(ActionCmd).Cmd().(fimage.PreviewPreparedMsg)
	require.NoError(t, second.Err)
	require.NotEqual(t, first.Generation, second.Generation)
	require.NotEqual(t, first.Key.Columns, second.Key.Columns)
}

func TestOverlayUpdateDialogDeliversPickerUpdateBelowFrontDialog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	require.NoError(t, os.WriteFile(path, png(), 0o600))
	picker := testPicker(t)
	picker.selectedPath = path
	top := &stubDialog{id: "top"}
	overlay := NewOverlay(picker, top)

	action := overlay.UpdateDialog(FilePickerID, FilePickerUpdateMsg{Capabilities: common.Capabilities{KittyGraphics: true, Columns: 100, Rows: 30, PixelX: 1000, PixelY: 600}, WindowSize: &tea.WindowSizeMsg{Width: 100, Height: 30}})
	prepared := action.(ActionCmd).Cmd().(fimage.PreviewPreparedMsg)

	require.Equal(t, fimage.EncodingKitty, prepared.Key.Encoding)
	require.Positive(t, prepared.Key.Columns)
	require.Positive(t, prepared.Key.Rows)
	require.Empty(t, top.received)
}

func TestFilePickerUpdateCapabilitiesInvalidatesStalePreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	require.NoError(t, os.WriteFile(path, png(), 0o600))
	picker := testPicker(t)
	picker.selectedPath = path
	overlay := NewOverlay(picker)

	first := overlay.Update(FilePickerUpdateMsg{WindowSize: &tea.WindowSizeMsg{Width: 100, Height: 30}}).(ActionCmd).Cmd().(fimage.PreviewPreparedMsg)
	overlay.Update(first)
	second := overlay.Update(FilePickerUpdateMsg{Capabilities: common.Capabilities{KittyGraphics: true, Columns: 100, Rows: 30, PixelX: 1000, PixelY: 600}}).(ActionCmd).Cmd().(fimage.PreviewPreparedMsg)

	require.Equal(t, fimage.EncodingKitty, second.Key.Encoding)
	require.NotEqual(t, first.Generation, second.Generation)
	require.NotEqual(t, first.Key, second.Key)
	overlay.Update(first)
	require.Empty(t, picker.preview)
}

func TestMetadataErrorsDoNotPopulateCache(t *testing.T) {
	picker := testPicker(t)
	picker.selectedPath = "missing.png"
	picker.generation = 1
	result := prepare(t, picker, picker.selectedPath)
	require.Error(t, result.Err)
	picker.HandleMsg(result)
	require.Empty(t, picker.cache)
	require.Empty(t, picker.preview)
}

// TestNanosecondMetadataChangesKey pins that PreviewKey's mtime field is
// fine-grained enough to notice a rewrite that leaves size unchanged: two
// otherwise-identical writes must still get different cache keys. The mtime
// is moved with os.Chtimes rather than by writing twice and hoping the wall
// clock advances between them, and by a millisecond rather than a single
// nanosecond — NTFS (Windows) stores mtimes at 100ns granularity, and a 1ns
// delta gets rounded away by the filesystem before Stat ever sees it. A
// millisecond stays far above that floor on every filesystem this runs on.
func TestNanosecondMetadataChangesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	require.NoError(t, os.WriteFile(path, png(), 0o600))
	picker := testPicker(t)
	picker.selectedPath = path
	picker.generation = 1
	first := prepare(t, picker, path)
	require.NoError(t, os.Chtimes(path, time.Now(), time.Unix(0, first.Key.ModTimeUnixNano+int64(time.Millisecond))))
	second := prepare(t, picker, path)
	require.NotEqual(t, first.Key, second.Key)
}

func TestStalePickerInstanceDoesNotApplyOrSendRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	require.NoError(t, os.WriteFile(path, png(), 0o600))
	old := testPicker(t)
	old.imgEnc = fimage.EncodingKitty
	old.selectedPath = path
	old.generation = 1
	result := prepare(t, old, path)
	require.NotEmpty(t, result.Output)

	current := testPicker(t)
	current.imgEnc = fimage.EncodingKitty
	current.selectedPath = path
	current.generation = result.Generation
	action := current.HandleMsg(result).(ActionCmd)

	require.Empty(t, current.preview)
	require.Nil(t, action.Cmd)
}

func TestPreviewCommandUsesImmutableSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	require.NoError(t, os.WriteFile(path, png(), 0o600))
	picker := testPicker(t)
	picker.selectedPath = path
	picker.generation = 1
	cmd := picker.preparePreviewCmd(picker.instance, path, picker.generation, picker.previewKey(path), picker.cacheSnapshot())
	picker.imgPrevWidth = 99
	result := cmd().(fimage.PreviewPreparedMsg)
	require.Equal(t, 2, result.Key.Columns)
}
