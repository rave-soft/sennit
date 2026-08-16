package dialog

import (
	"regexp"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// ThreadCreateID is the identifier for the thread-create dialog.
const ThreadCreateID = "thread-create"

// threadFieldHeight is the vertical space (label + input + spacing) each
// field takes in the dialog, mirroring Arguments' layout.
const threadFieldHeight = 3

// ActionCreateThread is returned once the user submits the dialog with a
// valid name. The caller (the main router) is responsible for actually
// creating the thread via workspace.CreateThread; this dialog only collects
// and validates input.
type ActionCreateThread struct {
	Name string
	Goal string
}

// ThreadCreate is a dialog for collecting the inputs needed to create a new
// thread: a name (normalized into a slug) and an optional goal.
type ThreadCreate struct {
	com     *common.Common
	name    textinput.Model
	goal    textinput.Model
	focused int // 0 = name, 1 = goal

	help   help.Model
	keyMap struct {
		Confirm,
		Next,
		Previous,
		Close key.Binding
	}
}

var _ Dialog = (*ThreadCreate)(nil)

// NewThreadCreate creates a new thread-create dialog.
func NewThreadCreate(com *common.Common) *ThreadCreate {
	s := &ThreadCreate{com: com}

	s.help = help.New()
	s.help.Styles = com.Styles.DialogHelpStyles()

	s.keyMap.Confirm = key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "confirm"),
	)
	s.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "tab"),
		key.WithHelp("↓/tab", "next"),
	)
	s.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "shift+tab"),
		key.WithHelp("↑/shift+tab", "previous"),
	)
	s.keyMap.Close = CloseKey

	name := textinput.New()
	name.SetVirtualCursor(false)
	name.SetStyles(com.Styles.TextInput)
	name.Prompt = "> "
	name.Placeholder = "thread-name"
	name.Focus()
	s.name = name

	goal := textinput.New()
	goal.SetVirtualCursor(false)
	goal.SetStyles(com.Styles.TextInput)
	goal.Prompt = "> "
	goal.Placeholder = "What should this thread accomplish?"
	goal.Blur()
	s.goal = goal

	return s
}

// ID implements Dialog.
func (s *ThreadCreate) ID() string {
	return ThreadCreateID
}

// input returns a pointer to the currently focused input.
func (s *ThreadCreate) input() *textinput.Model {
	if s.focused == 0 {
		return &s.name
	}
	return &s.goal
}

// focusField changes focus to the given index (0 or 1) with wrap-around.
func (s *ThreadCreate) focusField(newIndex int) {
	s.input().Blur()
	s.focused = ((newIndex % 2) + 2) % 2
	s.input().Focus()
}

// threadSlugDisallowed matches any run of characters that aren't lowercase
// letters, digits, or hyphens, once the name has been lowercased and had
// whitespace collapsed to hyphens.
var threadSlugDisallowed = regexp.MustCompile(`[^a-z0-9-]+`)

// threadSlugDashes collapses repeated hyphens left behind by stripping
// disallowed characters or collapsing whitespace.
var threadSlugDashes = regexp.MustCompile(`-{2,}`)

// threadSlugWhitespace matches runs of whitespace to collapse into a single
// hyphen before disallowed characters are stripped.
var threadSlugWhitespace = regexp.MustCompile(`\s+`)

// normalizeThreadName turns free-form input into a slug: lowercased, spaces
// replaced with hyphens, anything outside [a-z0-9-] stripped, repeated
// hyphens collapsed, and leading/trailing hyphens trimmed.
func normalizeThreadName(name string) string {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = threadSlugWhitespace.ReplaceAllString(slug, "-")
	slug = threadSlugDisallowed.ReplaceAllString(slug, "")
	slug = threadSlugDashes.ReplaceAllString(slug, "-")
	return strings.Trim(slug, "-")
}

// HandleMsg implements Dialog.
func (s *ThreadCreate) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, s.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, s.keyMap.Confirm):
			// Enter advances focus until the last field (goal), then submits.
			if s.focused != 1 {
				s.focusField(s.focused + 1)
				return nil
			}

			slug := normalizeThreadName(s.name.Value())
			if slug == "" {
				return ActionCmd{Cmd: util.ReportWarn("Thread name is required.")}
			}

			return ActionCreateThread{
				Name: slug,
				Goal: strings.TrimSpace(s.goal.Value()),
			}
		case key.Matches(msg, s.keyMap.Next):
			s.focusField(s.focused + 1)
		case key.Matches(msg, s.keyMap.Previous):
			s.focusField(s.focused - 1)
		default:
			var cmd tea.Cmd
			*s.input(), cmd = s.input().Update(msg)
			return ActionCmd{Cmd: cmd}
		}
	case tea.PasteMsg:
		var cmd tea.Cmd
		*s.input(), cmd = s.input().Update(msg)
		return ActionCmd{Cmd: cmd}
	}
	return nil
}

// Cursor returns the cursor position relative to the dialog.
func (s *ThreadCreate) Cursor() *tea.Cursor {
	cursor := InputCursor(s.com.Styles, s.input().Cursor())
	if cursor == nil {
		return nil
	}
	cursor.Y += s.focused * threadFieldHeight
	return cursor
}

// Draw implements Dialog.
//
// Two short fields plus a title and help footer comfortably fit even in
// small terminals, so unlike Arguments this dialog skips the viewport and
// scrollbar; width/height are still clamped to the drawable area per
// AGENTS.md.
func (s *ThreadCreate) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := s.com.Styles

	width := max(0, min(maxInputWidth, area.Dx()-t.Dialog.View.GetHorizontalFrameSize()))
	innerWidth := max(0, width-t.Dialog.Arguments.Content.GetHorizontalFrameSize())

	inputs := []struct {
		label string
		input *textinput.Model
	}{
		{"Name", &s.name},
		{"Goal", &s.goal},
	}

	var fields []string
	for i, f := range inputs {
		isFocused := i == s.focused

		labelStyle := t.Dialog.Arguments.InputLabelBlurred
		if isFocused {
			labelStyle = t.Dialog.Arguments.InputLabelFocused
		}
		label := labelStyle.Render(f.label)

		f.input.SetWidth(dialogInputTextWidth(t, *f.input, innerWidth))

		field := lipgloss.JoinVertical(lipgloss.Left, label, f.input.View(), "")
		fields = append(fields, field)
	}

	renderedFields := lipgloss.JoinVertical(lipgloss.Left, fields...)

	titleStyle := t.Dialog.Title
	header := common.DialogTitle(t, "New Thread", innerWidth, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)

	helpView := renderDialogHelp(t, &s.help, s, innerWidth)

	view := lipgloss.JoinVertical(
		lipgloss.Left,
		titleStyle.Render(header),
		t.Dialog.Arguments.Content.Render(renderedFields),
		helpView,
	)

	dialog := t.Dialog.View.Render(view)

	cur := s.Cursor()
	DrawCenterCursor(scr, area, dialog, cur)
	return cur
}

// ShortHelp implements help.KeyMap.
func (s *ThreadCreate) ShortHelp() []key.Binding {
	return []key.Binding{
		s.keyMap.Confirm,
		s.keyMap.Next,
		s.keyMap.Close,
	}
}

// FullHelp implements help.KeyMap.
func (s *ThreadCreate) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{s.keyMap.Confirm, s.keyMap.Next, s.keyMap.Previous},
		{s.keyMap.Close},
	}
}
