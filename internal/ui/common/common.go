package common

import (
	"context"
	"fmt"
	"image"
	"os"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/clipboard"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/rave-soft/braid/internal/workspace"
)

// MaxAttachmentSize defines the maximum allowed size for file attachments (5 MB).
const MaxAttachmentSize = int64(5 * 1024 * 1024)

// AllowedImageTypes defines the permitted image file types.
var AllowedImageTypes = []string{".jpg", ".jpeg", ".png"}

// Common defines common UI options and configurations.
type Common struct {
	Workspace workspace.Workspace
	Styles    *styles.Styles
	// Ctx is the process lifecycle context (typically the cobra command's
	// context, cancelled on interrupt/shutdown). The model and dialogs use
	// it for workspace calls issued from a tea.Cmd instead of
	// context.TODO(), so in-flight requests are cancelled with the
	// program rather than outliving it. Use Context() to read it, which
	// tolerates a zero-value Common (tests construct it without Ctx).
	Ctx context.Context
}

// Config returns the pure-data configuration associated with this [Common] instance.
func (c *Common) Config() *config.Config {
	return c.Workspace.Config()
}

// Context returns the lifecycle context for workspace calls, falling back
// to context.Background() when none was set (e.g. Common built directly by
// tests).
func (c *Common) Context() context.Context {
	if c.Ctx == nil {
		return context.Background()
	}
	return c.Ctx
}

// DefaultCommon returns the default common UI configurations. When the
// workspace has a model selected, the theme is chosen based on its
// provider; otherwise the default theme is used.
func DefaultCommon(ctx context.Context, ws workspace.Workspace) *Common {
	s := styles.ThemeForProvider(modelProviderID(ws))
	return &Common{
		Workspace: ws,
		Styles:    &s,
		Ctx:       ctx,
	}
}

// modelProviderID returns the provider ID of the currently selected model,
// or the empty string if none is set or the workspace is nil.
func modelProviderID(ws workspace.ConfigAccessor) string {
	if ws == nil {
		return ""
	}
	cfg := ws.Config()
	if cfg == nil {
		return ""
	}
	return cfg.Model.Provider
}

// CenterRect returns a new [Rectangle] centered within the given area with the
// specified width and height.
func CenterRect(area uv.Rectangle, width, height int) uv.Rectangle {
	centerX := area.Min.X + area.Dx()/2
	centerY := area.Min.Y + area.Dy()/2
	minX := centerX - width/2
	minY := centerY - height/2
	maxX := minX + width
	maxY := minY + height
	return image.Rect(minX, minY, maxX, maxY)
}

// BottomLeftRect returns a new [Rectangle] positioned at the bottom-left within the given area with the
// specified width and height.
func BottomLeftRect(area uv.Rectangle, width, height int) uv.Rectangle {
	minX := area.Min.X
	maxX := minX + width
	maxY := area.Max.Y
	minY := maxY - height
	return image.Rect(minX, minY, maxX, maxY)
}

// IsFileTooBig checks if the file at the given path exceeds the specified size
// limit.
func IsFileTooBig(filePath string, sizeLimit int64) (bool, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return false, fmt.Errorf("error getting file info: %w", err)
	}

	if fileInfo.Size() > sizeLimit {
		return true, nil
	}

	return false, nil
}

// CopyToClipboard copies the given text to the clipboard using both OSC 52
// (terminal escape sequence) and native clipboard for maximum compatibility.
// Returns a command that reports success to the user with the given message.
func CopyToClipboard(text, successMessage string) tea.Cmd {
	return CopyToClipboardWithCallback(text, successMessage, nil)
}

// CopyToClipboardWithCallback copies text to clipboard and executes a callback
// before showing the success message.
// This is useful when you need to perform additional actions like clearing UI state.
func CopyToClipboardWithCallback(text, successMessage string, callback tea.Cmd) tea.Cmd {
	return tea.Sequence(
		tea.SetClipboard(text),
		func() tea.Msg {
			clipboard.WriteText(text)
			return nil
		},
		callback,
		util.ReportInfo(successMessage),
	)
}
