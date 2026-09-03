package completions

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/sahilm/fuzzy"
)

// FileCompletionValue represents a file path completion value.
type FileCompletionValue struct {
	Path string
}

// ResourceCompletionValue represents a MCP resource completion value.
type ResourceCompletionValue struct {
	MCPName  string
	URI      string
	Title    string
	MIMEType string
}

// CommandCompletionValue represents a "/"-command completion value, sourced
// from the same commands (built-in, custom, and MCP prompts) shown in the
// Commands palette dialog. Action carries the dialog.Action to run when the
// command is picked; it's typed as `any` here so this package doesn't need
// to import internal/ui/dialog — the caller (internal/ui/model) does the
// type assertion back to dialog.Action.
type CommandCompletionValue struct {
	ID          string
	Title       string
	Aliases     []string
	Description string
	Shortcut    string
	Action      any
}

// CompletionItem represents an item in the completions list.
type CompletionItem struct {
	*list.Versioned

	text    string
	filter  string // what Filter() matches against; defaults to text
	desc    string // shown muted in the description column; never matched
	value   any
	match   fuzzy.Match
	focused bool
	cache   map[int]string

	// titleColumn is the column (from the start of the row) where the
	// description column begins, shared across every item currently visible
	// in the popup so descriptions line up — see Completions.alignColumns.
	// Zero disables the description column entirely (e.g. plain file/path
	// items, which have no description to show).
	titleColumn int

	// Styles
	normalStyle  lipgloss.Style
	focusedStyle lipgloss.Style
	matchStyle   lipgloss.Style
	mutedStyle   lipgloss.Style
}

// NewCompletionItem creates a new completion item.
func NewCompletionItem(text string, value any, normalStyle, focusedStyle, matchStyle lipgloss.Style) *CompletionItem {
	return &CompletionItem{
		Versioned:    list.NewVersioned(),
		text:         text,
		value:        value,
		normalStyle:  normalStyle,
		focusedStyle: focusedStyle,
		matchStyle:   matchStyle,
		cache:        make(map[int]string),
	}
}

// NewCommandCompletionItem creates a completion item for a "/" command. It
// displays the command's title, followed by its description in a muted,
// right-aligned column (see titleColumn), but filters against the title
// plus its aliases and description, so e.g. "clear" still surfaces "new".
func NewCommandCompletionItem(v CommandCompletionValue, normalStyle, focusedStyle, matchStyle, mutedStyle lipgloss.Style) *CompletionItem {
	item := NewCompletionItem(v.Title, v, normalStyle, focusedStyle, matchStyle)
	item.mutedStyle = mutedStyle
	item.desc = v.Description
	filter := v.Title
	if len(v.Aliases) > 0 {
		filter += " " + strings.Join(v.Aliases, " ")
	}
	if v.Description != "" {
		filter += " " + v.Description
	}
	item.filter = filter
	return item
}

// Finished implements list.Item. Completion items render purely from
// (text, match, focus); any mutation (SetMatch / SetFocused) bumps
// Version() so the frozen cache entry invalidates on the next
// render. Marking them finished lets the F6 list memo skip the
// per-line work for the steady completions popup.
func (c *CompletionItem) Finished() bool {
	return true
}

// Text returns the display text of the item.
func (c *CompletionItem) Text() string {
	return c.text
}

// Value returns the value of the item.
func (c *CompletionItem) Value() any {
	return c.value
}

// Filter implements [list.FilterableItem].
func (c *CompletionItem) Filter() string {
	if c.filter != "" {
		return c.filter
	}
	return c.text
}

// SetMatch implements [list.MatchSettable].
func (c *CompletionItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(c.match, m) {
		return
	}
	clear(c.cache)
	c.match = m
	c.Bump()
}

// sameFuzzyMatch reports whether two fuzzy.Match values are
// observably equal. Because Match contains a slice (MatchedIndexes)
// it is not directly comparable with ==; we compare the scalar
// fields and then walk the indexes. SetMatch uses this to skip
// gratuitous version bumps when the same match is reapplied.
func sameFuzzyMatch(a, b fuzzy.Match) bool {
	return a.Str == b.Str &&
		a.Index == b.Index &&
		a.Score == b.Score &&
		slices.Equal(a.MatchedIndexes, b.MatchedIndexes)
}

// SetFocused implements [list.Focusable].
func (c *CompletionItem) SetFocused(focused bool) {
	if c.focused == focused {
		return
	}
	clear(c.cache)
	c.focused = focused
	c.Bump()
}

// SetTitleColumn sets the column at which the description column starts.
// Called by Completions.alignColumns whenever the visible item set changes,
// so all items line up on the widest title currently shown.
func (c *CompletionItem) SetTitleColumn(col int) {
	if c.titleColumn == col {
		return
	}
	clear(c.cache)
	c.titleColumn = col
	c.Bump()
}

// SetStyles replaces the styles the item was built with and drops its
// render cache. [Completions.SetStyles] calls it on every item it holds so
// an open popup follows a live theme switch instead of staying in the
// palette it was opened in.
func (c *CompletionItem) SetStyles(normal, focused, match, muted lipgloss.Style) {
	c.normalStyle = normal
	c.focusedStyle = focused
	c.matchStyle = match
	c.mutedStyle = muted
	clear(c.cache)
	c.Bump()
}

// Render implements [list.Item].
func (c *CompletionItem) Render(width int) string {
	return renderItem(
		c.normalStyle,
		c.focusedStyle,
		c.matchStyle,
		c.mutedStyle,
		c.text,
		c.desc,
		c.titleColumn,
		c.focused,
		width,
		c.cache,
		&c.match,
	)
}

func renderItem(
	normalStyle, focusedStyle, matchStyle, mutedStyle lipgloss.Style,
	text, desc string,
	titleColumn int,
	focused bool,
	width int,
	cache map[int]string,
	match *fuzzy.Match,
) string {
	if cache == nil {
		cache = make(map[int]string)
	}

	cached, ok := cache[width]
	if ok {
		return cached
	}

	innerWidth := width - 2 // Account for padding
	// Truncate the title if needed. The title always wins the width budget;
	// the description (below) only gets what's left over, and is truncated
	// (or dropped) first.
	if ansi.StringWidth(text) > innerWidth {
		text = ansi.Truncate(text, innerWidth, "…")
	}
	titleWidth := ansi.StringWidth(text)

	// Select base style.
	style := normalStyle
	matchStyle = matchStyle.Background(style.GetBackground())
	mutedStyle = mutedStyle.Background(style.GetBackground())
	if focused {
		style = focusedStyle
		matchStyle = matchStyle.Background(style.GetBackground())
		mutedStyle = mutedStyle.Background(style.GetBackground())
	}

	// Pad the title out to the shared description column, then fit the
	// description into whatever's left. This is a separate rendered
	// segment — appended after the title is finalized — so match
	// highlighting (computed against the title's own byte offsets, below)
	// never bleeds into it.
	pad, fitted := fitColumnDescription(desc, titleWidth, titleColumn, innerWidth)

	// Render full-width text with background.
	content := style.Padding(0, 1).Width(width).Render(text + pad + fitted)

	// Apply match highlighting using StyleRanges. Filter() (what produced
	// match.MatchedIndexes) may include aliases/description text after the
	// title, which isn't part of what's displayed here — drop any index
	// past the title so highlighting never lands on the description column
	// or garbage past it.
	if len(match.MatchedIndexes) > 0 {
		var ranges []lipgloss.Range
		for _, rng := range util.MatchedRanges(match.MatchedIndexes) {
			if len(text) == 0 || rng[0] >= len(text) {
				continue
			}
			if rng[1] >= len(text) {
				rng[1] = len(text) - 1
			}
			start, stop := util.BytePosToVisibleCharPos(text, rng)
			// Offset by 1 for the padding space.
			ranges = append(ranges, lipgloss.NewRange(start+1, stop+2, matchStyle))
		}
		if len(ranges) > 0 {
			content = lipgloss.StyleRanges(content, ranges...)
		}
	}

	// Mute the description column, if any.
	if fitted != "" {
		start := titleColumn + 1 // +1 for the left padding column
		end := start + ansi.StringWidth(fitted)
		content = lipgloss.StyleRanges(content, lipgloss.NewRange(start, end, mutedStyle))
	}

	cache[width] = content
	return content
}

// fitColumnDescription returns the left-padding needed to push desc out to
// titleColumn (measured from the start of the — possibly already
// truncated — title), plus desc itself truncated to whatever width remains
// inside innerWidth. Both are "", "" when there's no description, no
// column alignment is active (titleColumn <= 0), or there's no room left
// for even a single truncated character. The title is never touched here;
// only the description gives way.
func fitColumnDescription(desc string, titleWidth, titleColumn, innerWidth int) (pad, fitted string) {
	if desc == "" || titleColumn <= 0 {
		return "", ""
	}

	budget := innerWidth - titleColumn
	if budget < 1 {
		return "", ""
	}

	fitted = desc
	if ansi.StringWidth(fitted) > budget {
		fitted = ansi.Truncate(fitted, budget, "…")
	}
	if fitted == "" {
		return "", ""
	}

	padWidth := max(0, titleColumn-titleWidth)
	return strings.Repeat(" ", padWidth), fitted
}

// Ensure CompletionItem implements the required interfaces.
var (
	_ list.Item           = (*CompletionItem)(nil)
	_ list.FilterableItem = (*CompletionItem)(nil)
	_ list.MatchSettable  = (*CompletionItem)(nil)
	_ list.Focusable      = (*CompletionItem)(nil)
)
