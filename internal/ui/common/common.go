package common

import (
	"context"
	"fmt"
	"image"
	"os"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/clipboard"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/uiprefs"
	"github.com/rave-soft/sennit/internal/workspace"
)

const MaxPreviewSize = int64(2 * 1024 * 1024)

// MaxAttachmentSize defines the maximum allowed size for file attachments (5 MB).
const MaxAttachmentSize = int64(5 * 1024 * 1024)

// AllowedImageTypes defines the permitted image file types.
var AllowedImageTypes = []string{".jpg", ".jpeg", ".png"}

type Workspace = workspace.FrontendWorkspace

// Common defines common UI options and configurations.
type Common struct {
	Workspace      Workspace
	SessionChanges workspace.SessionChangePreparer
	Styles         *styles.Styles
	// Prefs is where the UI reads and writes its own display preferences
	// (theme, compact mode, keybindings, and the rest of the fields
	// uiprefs.Prefs carries) instead of going through Workspace.Config().
	// See internal/uiprefs's package doc and CLIENT-SERVER.md "PR 0.5b":
	// once a remote daemon exists, these come from the client's own local
	// config rather than the server's merged one, so the UI must never
	// read them off Workspace.
	Prefs uiprefs.Store
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

// UIPrefs returns the current TUI display preferences. It is the read side
// of Prefs; components should call this instead of reaching into Config()
// for a display-only field.
func (c *Common) UIPrefs() uiprefs.Prefs {
	return c.Prefs.Prefs()
}

// Context returns the lifecycle context for workspace calls, falling back
// to context.Background() when none was set (e.g. Common built directly by
// tests).
func (c *Common) Context() context.Context {
	if c.Ctx == nil {
		return context.Background() //nolint:forbidigo // this *is* the fallback the lint rule tells callers to use instead
	}
	return c.Ctx
}

// DefaultCommon returns the default common UI configurations, styled with
// the theme the preference store selects (see the "/theme" command). An
// unset or unknown theme resolves to Sennit's default palette.
//
// prefs is variadic so the many tests that don't exercise a display
// preference don't have to thread one through: passing none defaults to a
// fresh, empty [uiprefs.MemStore]. Real callers (cmd/root.go, and
// model/root.go for threads and worktrees, which reuse the parent
// [Common]'s store) always pass one explicitly.
func DefaultCommon(ctx context.Context, ws Workspace, prefs ...uiprefs.Store) *Common {
	store := firstPrefsStore(prefs)
	s := styles.Theme(ThemeID(store)).WithSpinner(SpinnerMode(store))
	sessionChanges, _ := ws.(workspace.SessionChangePreparer)
	return &Common{
		Workspace:      ws,
		SessionChanges: sessionChanges,
		Styles:         &s,
		Prefs:          store,
		Ctx:            ctx,
	}
}

// firstPrefsStore returns the first store passed to [DefaultCommon], or a
// fresh [uiprefs.MemStore] when none was.
func firstPrefsStore(prefs []uiprefs.Store) uiprefs.Store {
	if len(prefs) > 0 && prefs[0] != nil {
		return prefs[0]
	}
	return &uiprefs.MemStore{}
}

// ThemeID returns the theme configured in prefs, or the empty string when
// there is no store yet — both of which styles.Theme maps onto the default
// palette.
func ThemeID(prefs uiprefs.Store) string {
	if prefs == nil {
		return ""
	}
	return prefs.Prefs().ThemeID
}

// SpinnerMode returns the working-indicator motion configured in prefs,
// defaulting when there is no store yet. An unrecognised value resolves to
// the default here and is reported as a config problem by the doctor, not
// by refusing to render.
func SpinnerMode(prefs uiprefs.Store) spin.Mode {
	if prefs == nil {
		return spin.ModeScramble
	}
	return styles.SpinnerMode(prefs.Prefs().SpinnerMode)
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
