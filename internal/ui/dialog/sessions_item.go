package dialog

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/dustin/go-humanize"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/sahilm/fuzzy"
)

// ListItem represents a selectable and searchable item in a dialog list.
type ListItem interface {
	list.FilterableItem
	list.Focusable
	list.MatchSettable

	// ID returns the unique identifier of the item.
	ID() string
}

// SessionItem wraps a [session.Session] to implement the [ListItem] interface.
type SessionItem struct {
	list.BaseItem
	session.Session
	t                *styles.Styles
	sessionsMode     sessionsMode
	updateTitleInput textinput.Model
	hideInfo         bool
}

var _ ListItem = &SessionItem{}

// Filter returns the filterable value of the session.
func (s *SessionItem) Filter() string {
	return s.Title
}

// ID returns the unique identifier of the session.
func (s *SessionItem) ID() string {
	return s.Session.ID
}

// InputValue returns the updated title value
func (s *SessionItem) InputValue() string {
	return s.updateTitleInput.Value()
}

// HandleInput forwards input message to the update title input
func (s *SessionItem) HandleInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	s.updateTitleInput, cmd = s.updateTitleInput.Update(msg)
	s.Invalidate()
	return cmd
}

// Cursor returns the cursor of the update title input
func (s *SessionItem) Cursor() *tea.Cursor {
	return s.updateTitleInput.Cursor()
}

// InfoText returns the secondary text shown on the right of the item.
func (s *SessionItem) InfoText() string {
	return humanize.Time(time.Unix(s.UpdatedAt, 0))
}

// SetHideInfo controls whether the timestamp info column is shown. The
// dialog hides it uniformly when it would crowd the title.
func (s *SessionItem) SetHideInfo(v bool) {
	if s.hideInfo == v {
		return
	}
	s.hideInfo = v
	s.Invalidate()
}

// Render returns the string representation of the session item.
func (s *SessionItem) Render(width int) string {
	info := s.InfoText()
	if s.hideInfo {
		info = ""
	}
	styles := listItemStylesWithInfo(s.t, s.t.Dialog.Sessions.InfoBlurred, s.t.Dialog.Sessions.InfoFocused)

	switch s.sessionsMode {
	case sessionsModeDeleting:
		styles.ItemBlurred = s.t.Dialog.Sessions.DeletingItemBlurred
		styles.ItemFocused = s.t.Dialog.Sessions.DeletingItemFocused
	case sessionsModeUpdating:
		styles.ItemBlurred = s.t.Dialog.Sessions.RenamingItemBlurred
		styles.ItemFocused = s.t.Dialog.Sessions.RenamingingItemFocused
		if s.Focused() {
			const cursorPadding = 1
			inputWidth := max(0, width-styles.ItemFocused.GetHorizontalFrameSize()-cursorPadding)
			s.updateTitleInput.SetWidth(inputWidth)
			s.updateTitleInput.Placeholder = ansi.Truncate(s.Title, width, "…")
			return styles.ItemFocused.Render(s.updateTitleInput.View())
		}
	}

	return renderItem(styles, s.Title, info, s.Focused(), width, s.Cache(), s.Match())
}

// defaultListItemStyles builds the standard ListItemStyles used by nearly
// every dialog's item type: the shared normal/selected item styles paired
// with the shared Dialog.ListItem info-text styles.
func defaultListItemStyles(t *styles.Styles) ListItemStyles {
	return listItemStylesWithInfo(t, t.Dialog.ListItem.InfoBlurred, t.Dialog.ListItem.InfoFocused)
}

// listItemStylesWithInfo builds a ListItemStyles from the shared item
// styles plus caller-supplied info-text styles. Sessions is the one item
// type whose info text uses its own palette (Dialog.Sessions) instead of
// the shared Dialog.ListItem one, so it calls this directly.
func listItemStylesWithInfo(t *styles.Styles, infoBlurred, infoFocused lipgloss.Style) ListItemStyles {
	return ListItemStyles{
		ItemBlurred:     t.Dialog.NormalItem,
		ItemFocused:     t.Dialog.SelectedItem,
		InfoTextBlurred: infoBlurred,
		InfoTextFocused: infoFocused,
	}
}

type ListItemStyles struct {
	ItemBlurred     lipgloss.Style
	ItemFocused     lipgloss.Style
	InfoTextBlurred lipgloss.Style
	InfoTextFocused lipgloss.Style
}

func renderItem(t ListItemStyles, title string, info string, focused bool, width int, cache map[int]string, m *fuzzy.Match) string {
	if cache == nil {
		cache = make(map[int]string)
	}

	cached, ok := cache[width]
	if ok {
		return cached
	}

	style := t.ItemBlurred
	if focused {
		style = t.ItemFocused
	}

	// Build content to the width left after the item style's own padding,
	// so the final rendered line is exactly `width` wide. Otherwise the
	// padding pushes the line past the list width and it wraps.
	lineWidth := max(0, width-style.GetHorizontalFrameSize())

	var infoText string
	var infoWidth int
	if len(info) > 0 {
		// Cap the info column so a long value (e.g. a provider name) can
		// truncate instead of overflowing the row or squeezing the title
		// to nothing; the title keeps at least half the width.
		if maxInfo := lineWidth / 2; lipgloss.Width(info)+2 > maxInfo {
			info = ansi.Truncate(info, max(0, maxInfo-2), "…")
		}
		infoText = fmt.Sprintf(" %s ", info)
		if focused {
			infoText = t.InfoTextFocused.Render(infoText)
		} else {
			infoText = t.InfoTextBlurred.Render(infoText)
		}

		infoWidth = lipgloss.Width(infoText)
	}

	title = ansi.Truncate(title, max(0, lineWidth-infoWidth), "…")
	titleWidth := lipgloss.Width(title)
	gap := strings.Repeat(" ", max(0, lineWidth-titleWidth-infoWidth))
	content := title
	if m != nil && len(m.MatchedIndexes) > 0 {
		var lastPos int
		parts := make([]string, 0)
		ranges := util.MatchedRanges(m.MatchedIndexes)
		for _, rng := range ranges {
			start, stop := util.BytePosToVisibleCharPos(title, rng)
			if start > lastPos {
				parts = append(parts, ansi.Cut(title, lastPos, start))
			}
			// NOTE: We're using [ansi.Style] here instead of [lipglosStyle]
			// because we can control the underline start and stop more
			// precisely via [ansi.AttrUnderline] and [ansi.AttrNoUnderline]
			// which only affect the underline attribute without interfering
			// with other style attributes.
			parts = append(
				parts,
				ansi.NewStyle().Underline(true).String(),
				ansi.Cut(title, start, stop+1),
				ansi.NewStyle().Underline(false).String(),
			)
			lastPos = stop + 1
		}
		if lastPos < ansi.StringWidth(title) {
			parts = append(parts, ansi.Cut(title, lastPos, ansi.StringWidth(title)))
		}

		content = strings.Join(parts, "")
	}

	content = style.Render(content + gap + infoText)
	cache[width] = content
	return content
}

// sessionItems takes a slice of [session.Session]s and convert them to a slice
// of [ListItem]s.
func sessionItems(t *styles.Styles, mode sessionsMode, sessions ...session.Session) []list.FilterableItem {
	items := make([]list.FilterableItem, len(sessions))
	for i, s := range sessions {
		item := &SessionItem{BaseItem: list.NewBaseItem(), Session: s, t: t, sessionsMode: mode}
		if mode == sessionsModeUpdating {
			item.updateTitleInput = textinput.New()
			item.updateTitleInput.SetVirtualCursor(false)
			item.updateTitleInput.Prompt = ""
			inputStyle := t.TextInput
			inputStyle.Focused.Placeholder = t.Dialog.Sessions.RenamingPlaceholder
			item.updateTitleInput.SetStyles(inputStyle)
			item.updateTitleInput.Focus()
		}
		items[i] = item
	}
	return items
}
