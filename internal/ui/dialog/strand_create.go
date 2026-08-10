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
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/util"
)

// StrandCreateID is the identifier for the strand-create dialog.
const StrandCreateID = "strand-create"

// strandFieldHeight is the vertical space (label + input + spacing) each
// field takes in the dialog, mirroring Arguments' layout.
const strandFieldHeight = 3

// ActionCreateStrand is returned once the user submits the dialog with a
// valid name. The caller (the main router) is responsible for actually
// creating the strand via workspace.CreateStrand; this dialog only collects
// and validates input.
type ActionCreateStrand struct {
	Name string
	Goal string
}

// StrandCreate is a dialog for collecting the inputs needed to create a new
// strand: a name (normalized into a slug) and an optional goal.
type StrandCreate struct {
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

var _ Dialog = (*StrandCreate)(nil)

// NewStrandCreate creates a new strand-create dialog.
func NewStrandCreate(com *common.Common) *StrandCreate {
	s := &StrandCreate{com: com}

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
	name.Placeholder = "strand-name"
	name.Focus()
	s.name = name

	goal := textinput.New()
	goal.SetVirtualCursor(false)
	goal.SetStyles(com.Styles.TextInput)
	goal.Prompt = "> "
	goal.Placeholder = "What should this strand accomplish?"
	goal.Blur()
	s.goal = goal

	return s
}

// ID implements Dialog.
func (s *StrandCreate) ID() string {
	return StrandCreateID
}

// input returns a pointer to the currently focused input.
func (s *StrandCreate) input() *textinput.Model {
	if s.focused == 0 {
		return &s.name
	}
	return &s.goal
}

// focusField changes focus to the given index (0 or 1) with wrap-around.
func (s *StrandCreate) focusField(newIndex int) {
	s.input().Blur()
	s.focused = ((newIndex % 2) + 2) % 2
	s.input().Focus()
}

// strandSlugDisallowed matches any run of characters that aren't lowercase
// letters, digits, or hyphens, once the name has been lowercased and had
// whitespace collapsed to hyphens.
var strandSlugDisallowed = regexp.MustCompile(`[^a-z0-9-]+`)

// strandSlugDashes collapses repeated hyphens left behind by stripping
// disallowed characters or collapsing whitespace.
var strandSlugDashes = regexp.MustCompile(`-{2,}`)

// strandSlugWhitespace matches runs of whitespace to collapse into a single
// hyphen before disallowed characters are stripped.
var strandSlugWhitespace = regexp.MustCompile(`\s+`)

// normalizeStrandName turns free-form input into a slug: lowercased, spaces
// replaced with hyphens, anything outside [a-z0-9-] stripped, repeated
// hyphens collapsed, and leading/trailing hyphens trimmed.
func normalizeStrandName(name string) string {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = strandSlugWhitespace.ReplaceAllString(slug, "-")
	slug = strandSlugDisallowed.ReplaceAllString(slug, "")
	slug = strandSlugDashes.ReplaceAllString(slug, "-")
	return strings.Trim(slug, "-")
}

// HandleMsg implements Dialog.
func (s *StrandCreate) HandleMsg(msg tea.Msg) Action {
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

			slug := normalizeStrandName(s.name.Value())
			if slug == "" {
				return ActionCmd{Cmd: util.ReportWarn("Strand name is required.")}
			}

			return ActionCreateStrand{
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
func (s *StrandCreate) Cursor() *tea.Cursor {
	cursor := InputCursor(s.com.Styles, s.input().Cursor())
	if cursor == nil {
		return nil
	}
	cursor.Y += s.focused * strandFieldHeight
	return cursor
}

// Draw implements Dialog.
//
// Two short fields plus a title and help footer comfortably fit even in
// small terminals, so unlike Arguments this dialog skips the viewport and
// scrollbar; width/height are still clamped to the drawable area per
// AGENTS.md.
func (s *StrandCreate) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
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
	header := common.DialogTitle(t, "New Strand", innerWidth, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)

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
func (s *StrandCreate) ShortHelp() []key.Binding {
	return []key.Binding{
		s.keyMap.Confirm,
		s.keyMap.Next,
		s.keyMap.Close,
	}
}

// FullHelp implements help.KeyMap.
func (s *StrandCreate) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{s.keyMap.Confirm, s.keyMap.Next, s.keyMap.Previous},
		{s.keyMap.Close},
	}
}
