package dialog

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"maps"
	"os"
	"strings"
	"sync/atomic"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/ui/common"
	fimage "github.com/rave-soft/sennit/internal/ui/image"
)

const FilePickerID = "filepicker"

var filePickerInstance atomic.Uint64

type FilePickerUpdateMsg struct {
	Capabilities common.Capabilities
	WindowSize   *tea.WindowSizeMsg
}

type FilePicker struct {
	Base
	com *common.Common

	fp   filepicker.Model
	help help.Model

	imgEnc        fimage.Encoding
	imgPrevWidth  int
	imgPrevHeight int
	cellSizeW     int
	cellSizeH     int
	isTmux        bool

	instance     uint64
	selectedPath string
	generation   uint64
	preview      string
	cache        map[fimage.PreviewKey]string

	km struct {
		Select, Down, Up, Forward, Backward, Navigate, Close key.Binding
	}
}

var _ Dialog = (*FilePicker)(nil)

func NewFilePicker(com *common.Common) (*FilePicker, tea.Cmd) {
	f := &FilePicker{Base: NewBase(com, filePickerMinWidth), com: com, instance: filePickerInstance.Add(1), cache: make(map[fimage.PreviewKey]string)}
	helpModel := help.New()
	helpModel.Styles = com.Styles.DialogHelpStyles()
	f.help = helpModel
	f.km.Select = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "accept"))
	f.km.Down = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("down/j", "move down"))
	f.km.Up = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("up/k", "move up"))
	f.km.Forward = key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("right/l", "move forward"))
	f.km.Backward = key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("left/h", "move backward"))
	f.km.Navigate = key.NewBinding(key.WithKeys("right", "l", "left", "h", "up", "k", "down", "j"), key.WithHelp("↑↓←→", "navigate"))
	f.km.Close = key.NewBinding(key.WithKeys("esc", "alt+esc"), key.WithHelp("esc", "close/exit"))
	fp := filepicker.New()
	fp.AllowedTypes = common.AllowedImageTypes
	fp.ShowPermissions = false
	fp.ShowSize = false
	fp.AutoHeight = false
	fp.Styles = com.Styles.FilePicker
	fp.Cursor = ""
	fp.CurrentDirectory = f.WorkingDir()
	f.fp = fp
	return f, f.fp.Init()
}

func (f *FilePicker) SetImageCapabilities(caps *common.Capabilities) {
	f.setImageCapabilities(caps)
}

func (f *FilePicker) setImageCapabilities(caps *common.Capabilities) bool {
	encoding := fimage.EncodingBlocks
	cellSizeW, cellSizeH := 0, 0
	isTmux := false
	if caps != nil {
		if caps.SupportsKittyGraphics() {
			encoding = fimage.EncodingKitty
		}
		cellSizeW, cellSizeH = caps.CellSize()
		_, isTmux = caps.Env.LookupEnv("TMUX")
	}
	if encoding == f.imgEnc && cellSizeW == f.cellSizeW && cellSizeH == f.cellSizeH && isTmux == f.isTmux {
		return false
	}
	f.imgEnc, f.cellSizeW, f.cellSizeH, f.isTmux = encoding, cellSizeW, cellSizeH, isTmux
	f.invalidatePreview()
	return true
}

func (f *FilePicker) WorkingDir() string {
	if f.com != nil && f.com.Workspace != nil {
		if wd := f.com.Workspace.WorkingDir(); wd != "" {
			return wd
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return home.Dir()
	}
	return cwd
}

func (f *FilePicker) ShortHelp() []key.Binding {
	return []key.Binding{f.km.Navigate, f.km.Select, f.km.Close}
}

func (f *FilePicker) FullHelp() [][]key.Binding {
	return [][]key.Binding{{f.km.Select, f.km.Down, f.km.Up, f.km.Forward}, {f.km.Backward, f.km.Close}}
}

func (f *FilePicker) ID() string { return FilePickerID }

func (f *FilePicker) HandleMsg(msg tea.Msg) Action {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && key.Matches(keyMsg, f.km.Close) {
		return ActionClose{}
	}
	if prepared, ok := msg.(fimage.PreviewPreparedMsg); ok {
		return f.handlePreviewPrepared(prepared)
	}
	previewChanged := false
	switch update := msg.(type) {
	case FilePickerUpdateMsg:
		previewChanged = f.setImageCapabilities(&update.Capabilities)
		if update.WindowSize != nil && f.setPreviewSize(update.WindowSize.Width, update.WindowSize.Height) {
			f.invalidatePreview()
			previewChanged = true
		}
		if previewChanged {
			return f.preparePreviewAction()
		}
		return ActionCmd{}
	}
	var cmds []tea.Cmd
	var cmd tea.Cmd
	f.fp, cmd = f.fp.Update(msg)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	if selected, path := f.fp.DidSelectFile(msg); selected {
		return ActionFilePickerSelected{Path: path}
	}
	if path := f.fp.HighlightedPath(); path != f.selectedPath {
		f.selectedPath = path
		f.invalidatePreview()
		previewChanged = true
	}
	if previewChanged && f.isImagePath(f.selectedPath) {
		cmds = append(cmds, f.preparePreviewCmd(f.instance, f.selectedPath, f.generation, f.previewKey(f.selectedPath), f.cacheSnapshot()))
	}
	if len(cmds) == 0 {
		return ActionCmd{}
	}
	return ActionCmd{Cmd: tea.Batch(cmds...)}
}

func (f *FilePicker) preparePreviewAction() Action {
	if !f.isImagePath(f.selectedPath) {
		return ActionCmd{}
	}
	return ActionCmd{Cmd: f.preparePreviewCmd(f.instance, f.selectedPath, f.generation, f.previewKey(f.selectedPath), f.cacheSnapshot())}
}

func (f *FilePicker) handlePreviewPrepared(msg fimage.PreviewPreparedMsg) Action {
	if msg.Instance != f.instance || msg.Generation != f.generation || msg.Key.Path != f.selectedPath {
		return ActionCmd{}
	}
	if msg.Err != nil {
		f.preview = ""
		return ActionCmd{}
	}
	f.cache[msg.Key] = msg.Rendered
	f.preview = msg.Rendered
	if msg.Output == "" {
		return ActionCmd{}
	}
	return ActionCmd{Cmd: tea.Raw(msg.Output)}
}

func (f *FilePicker) preparePreviewCmd(instance uint64, path string, generation uint64, requested fimage.PreviewKey, cache map[fimage.PreviewKey]string) tea.Cmd {
	return func() tea.Msg {
		info, err := os.Stat(path)
		if err != nil {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: requested, Generation: generation, Err: fmt.Errorf("stat image: %w", err)}
		}
		key := requested
		key.Size = info.Size()
		key.ModTimeUnixNano = info.ModTime().UnixNano()
		if info.IsDir() {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Err: fmt.Errorf("image path is a directory")}
		}
		if info.Size() > common.MaxPreviewSize {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Err: fmt.Errorf("image exceeds preview size limit")}
		}
		if rendered, ok := cache[key]; ok {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Rendered: rendered}
		}
		file, err := os.Open(path)
		if err != nil {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Err: fmt.Errorf("open image: %w", err)}
		}
		defer file.Close()

		// A byte-size cap alone doesn't bound decoded memory: a solid-color
		// PNG can compress a huge canvas into a couple of megabytes on disk
		// yet expand to gigabytes of RGBA once decoded. Check the declared
		// pixel count before doing that decode.
		cfg, _, err := image.DecodeConfig(file)
		if err != nil {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Err: fmt.Errorf("decode image config: %w", err)}
		}
		if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > maxPreviewPixels {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Err: fmt.Errorf("image exceeds preview pixel limit")}
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Err: fmt.Errorf("seek image: %w", err)}
		}
		img, _, err := image.Decode(file)
		if err != nil {
			return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Err: fmt.Errorf("decode image: %w", err)}
		}
		rendered, output, err := fimage.Prepare(key, img)
		return fimage.PreviewPreparedMsg{Instance: instance, Key: key, Generation: generation, Rendered: rendered, Output: output, Err: err}
	}
}

func (f *FilePicker) cacheSnapshot() map[fimage.PreviewKey]string {
	cache := make(map[fimage.PreviewKey]string, len(f.cache))
	maps.Copy(cache, f.cache)
	return cache
}

func (f *FilePicker) previewKey(path string) fimage.PreviewKey {
	return fimage.PreviewKey{Path: path, Columns: f.imgPrevWidth, Rows: f.imgPrevHeight, Encoding: f.imgEnc, CellSize: fimage.CellSize{Width: f.cellSizeW, Height: f.cellSizeH}, Tmux: f.isTmux}
}

func (f *FilePicker) invalidatePreview() {
	f.generation++
	f.preview = ""
}

func (f *FilePicker) isImagePath(path string) bool {
	path = strings.ToLower(path)
	for _, extension := range f.fp.AllowedTypes {
		if strings.HasSuffix(path, extension) {
			return true
		}
	}
	return false
}

func (f *FilePicker) setPreviewSize(screenWidth, screenHeight int) bool {
	t := f.com.Styles
	width := max(0, min(filePickerMinWidth, screenWidth-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(10, screenHeight-t.Dialog.View.GetVerticalBorderSize()))
	previewHeight := 0
	if height > 5 {
		previewHeight = filePickerMinHeight*2 - t.Dialog.ImagePreview.GetVerticalFrameSize()
	}
	previewWidth := width - t.Dialog.View.GetHorizontalFrameSize() - t.Dialog.ImagePreview.GetHorizontalFrameSize()
	if previewWidth == f.imgPrevWidth && previewHeight == f.imgPrevHeight {
		return false
	}
	f.imgPrevWidth, f.imgPrevHeight = previewWidth, previewHeight
	return true
}

const (
	filePickerMinWidth  = 70
	filePickerMinHeight = 10
)

// maxPreviewPixels bounds decoded image dimensions for the preview pane.
// 24 megapixels covers a typical high-res photo (e.g. 6000x4000) at
// 4 bytes/px RGBA (~96MB decoded); well above that, a preview is not worth
// the memory a pathological or hostile file can force us to allocate.
const maxPreviewPixels = 24_000_000

func (f *FilePicker) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := f.com.Styles
	f.Resize(area)
	width := f.Width()
	height := max(0, min(10, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := f.InnerWidth()
	f.fp.SetHeight(height)
	styles := t.FilePicker
	styles.File = styles.File.Width(innerWidth)
	styles.Directory = styles.Directory.Width(innerWidth)
	styles.Selected = styles.Selected.PaddingLeft(1).Width(innerWidth)
	styles.DisabledSelected = styles.DisabledSelected.PaddingLeft(1).Width(innerWidth)
	f.fp.Styles = styles
	rc := NewRenderContext(t, width)
	rc.Gap = 1
	rc.Title = "Add Image"
	rc.Help = renderDialogHelp(t, &f.help, f, innerWidth)
	if f.imgPrevHeight > 0 {
		rc.AddPart(t.Dialog.ImagePreview.Align(lipgloss.Center).Width(innerWidth).Render(f.imagePreview()))
	}
	rc.AddPart(strings.TrimSpace(f.fp.View()))
	DrawCenter(scr, area, rc.Render())
	return nil
}

func (f *FilePicker) imagePreview() string {
	if f.preview != "" {
		return f.preview
	}
	var output strings.Builder
	for y := range f.imgPrevHeight {
		for range f.imgPrevWidth {
			output.WriteRune('█')
		}
		if y < f.imgPrevHeight-1 {
			output.WriteByte('\n')
		}
	}
	return output.String()
}
