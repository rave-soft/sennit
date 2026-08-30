package chat

import (
	"fmt"
	"strings"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// shellSeq provides unique IDs for ShellItems even when the same
// command is run multiple times.
var shellSeq atomic.Int64

const (
	shellMaxCollapsedLines = 10
	shellHScrollStep       = 5
)

// ShellItem renders a bang-mode shell command result in the chat with a
// vertical bar on the left and plain-text output.
type ShellItem struct {
	*list.Versioned
	*highlightableMessageItem
	*cachedMessageItem
	*focusableMessageItem

	id              string
	command         string
	output          strings.Builder
	exitCode        int
	expandedContent bool
	xOffset         int
	maxLineWidth    int // computed during render, used to clamp xOffset
	sty             *styles.Styles
	pending         bool
	anim            *spin.Anim
	hovered         bool
}

var (
	_ Expandable         = (*ShellItem)(nil)
	_ list.Highlightable = (*ShellItem)(nil)
	_ KeyEventHandler    = (*ShellItem)(nil)
	_ Animatable         = (*ShellItem)(nil)
)

// NewShellItem creates a new ShellItem for displaying bang-mode results.
func NewShellItem(sty *styles.Styles, command, output string, exitCode int) MessageItem {
	v := list.NewVersioned()
	s := &ShellItem{
		Versioned:                v,
		highlightableMessageItem: defaultHighlighter(sty, v),
		cachedMessageItem:        &cachedMessageItem{},
		focusableMessageItem:     newFocusableMessageItem(v),
		id:                       fmt.Sprintf("shell-%d-%s", shellSeq.Add(1), command),
		command:                  command,
		exitCode:                 exitCode,
		sty:                      sty,
	}
	s.replaceOutput(output)
	return s
}

// NewPendingShellItem creates a ShellItem in a pending/running state that
// displays a spinner until Complete is called with the results.
func NewPendingShellItem(sty *styles.Styles, command string) *ShellItem {
	v := list.NewVersioned()
	id := fmt.Sprintf("shell-%d-%s", shellSeq.Add(1), command)
	s := &ShellItem{
		Versioned:                v,
		highlightableMessageItem: defaultHighlighter(sty, v),
		cachedMessageItem:        &cachedMessageItem{},
		focusableMessageItem:     newFocusableMessageItem(v),
		id:                       id,
		command:                  command,
		sty:                      sty,
		pending:                  true,
	}
	s.anim = s.newAnim()
	return s
}

// newAnim builds the "Running" animation from the item's current styles.
// Construction and [ShellItem.Restyle] share it so a rebuilt animation
// cannot drift from the original settings.
func (s *ShellItem) newAnim() *spin.Anim {
	return spin.New(spin.Settings{
		ID:         s.id,
		Label:      "Running",
		LabelColor: s.sty.WorkingLabelColor,
		GradColorA: s.sty.WorkingGradFromColor,
		GradColorB: s.sty.WorkingGradToColor,
		// Always ModeNone regardless of the configured spinner: a
		// shell command is running, not thinking, and the scrambled
		// glyphs say the opposite. The person's spinner preference
		// governs the LLM-work spinners, which is where the choice is
		// about how much motion they want to sit next to.
		Mode: spin.ModeNone,
	})
}

// Restyle implements [Restylable]. See [AssistantMessageItem.Restyle].
func (s *ShellItem) Restyle() tea.Cmd {
	if !s.pending {
		return nil
	}
	s.anim = s.newAnim()
	s.Bump()
	return s.StartAnimation()
}

// Complete transitions a pending ShellItem to a finished state with output.
func (s *ShellItem) Complete(output string, exitCode int) {
	s.replaceOutput(output)
	s.exitCode = exitCode
	s.pending = false
	s.Bump()
}

// AppendOutput appends incremental output to a pending ShellItem.
func (s *ShellItem) AppendOutput(chunk string) {
	if !s.pending {
		return
	}
	s.output.WriteString(chunk)
	s.Bump()
}

func (s *ShellItem) replaceOutput(output string) {
	s.output.Reset()
	s.output.Grow(len(output))
	s.output.WriteString(output)
}

func (s *ShellItem) ID() string          { return s.id }
func (s *ShellItem) FilterValue() string { return s.command }
func (s *ShellItem) Finished() bool      { return !s.pending }

// StartAnimation starts the spinner animation for pending shell items.
func (s *ShellItem) StartAnimation() tea.Cmd {
	if !s.pending {
		return nil
	}
	return s.anim.Start()
}

// Animate advances the spinner animation for pending shell items.
func (s *ShellItem) Animate(msg spin.StepMsg) tea.Cmd {
	if !s.pending {
		return nil
	}
	s.Bump()
	return s.anim.Animate(msg)
}

// Render implements MessageItem. Unlike the other item types, this does
// not go through the prefixed-render cache, and RawRender itself is
// uncached too: it depends on width, xOffset, expandedContent, hovered,
// focused, and the streamed output, and while pending it also changes
// every animation frame. A getCachedRender(width) key, the only kind
// cachedMessageItem offers, cannot capture that without every mutator
// (AppendOutput, ScrollHorizontal, SetHovered, ToggleExpanded, the
// spinner tick, ...) remembering to invalidate it — a stale frame here
// would show scrolled-away or already-replaced output, which is worse
// than the recompute cost of a plain-text remap.
func (s *ShellItem) Render(width int) string {
	// RawRender applies cappedMessageWidth itself (see tools_item.go's
	// RawRender for the same contract): it takes the raw item width, not
	// one already reduced by MessageLeftPaddingTotal. Pre-subtracting
	// here used to make output and the xOffset scroll clamp two columns
	// narrower than the space actually available.
	content := s.RawRender(width)

	// Highlight before prefixing, not after. SetHighlight's stored columns
	// are already shifted left by MessageLeftPaddingTotal to address the
	// unprefixed content (see its comment) — assistant.go, user.go and
	// tools_item.go all highlight for that same reason before adding their
	// prefix. Highlighting the already-prefixed string here used to land
	// the selection two columns short of the mouse, onto the bar/padding,
	// and left copied text (via RawRender, read by the same columns) not
	// matching what was shown as selected.
	content = s.renderHighlighted(content, cappedMessageWidth(width), lipgloss.Height(content))

	var prefix string
	if s.focused {
		prefix = s.sty.Messages.ShellBarFocused.Render()
	} else {
		prefix = s.sty.Messages.ShellBarBlurred.Render()
	}
	return prefixLines(content, prefix)
}

// HandleMouseClick implements MouseClickable so clicks select this item.
func (s *ShellItem) HandleMouseClick(btn ansi.MouseButton, x, y int) bool {
	return btn == ansi.MouseLeft && s.HoverableAt(x, y, 0)
}

// HoverableAt reports whether the output block can visibly expand or collapse.
func (s *ShellItem) HoverableAt(_ int, y, _ int) bool {
	raw := trimShellOutput(s.output.String())
	return raw != "" && strings.Count(raw, "\n")+1 > shellMaxCollapsedLines && y > 0
}

// trimShellOutput strips trailing whitespace and bare ANSI resets that
// programs like `task` emit after their last line of output. Shared by
// RawRender and HoverableAt so the line count that decides whether a
// block is expandable matches the one that decides whether it's
// clickable — using two different trims (e.g. TrimSpace vs. this) can
// disagree on how many lines the output has and leave a block
// clickable but unable to actually expand, or vice versa.
func trimShellOutput(output string) string {
	raw := output
	for {
		trimmed := strings.TrimRight(raw, " \t\r\n")
		trimmed = strings.TrimSuffix(trimmed, "\x1b[0m")
		if trimmed == raw {
			break
		}
		raw = trimmed
	}
	return raw
}

// SetHovered updates shell-output hover feedback.
func (s *ShellItem) SetHovered(hovered bool) {
	if s.hovered == hovered {
		return
	}
	s.hovered = hovered
	s.clearCache()
	s.Bump()
}

// HandleKeyEvent implements KeyEventHandler for copy and horizontal scrolling.
func (s *ShellItem) HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd) {
	switch k := key.String(); k {
	case "c", "y":
		text := "$ " + s.command + "\n" + ansi.Strip(s.output.String())
		return true, common.CopyToClipboard(text, "Shell output copied to clipboard")
	case "shift+left", "H":
		if s.xOffset > 0 {
			s.xOffset = max(0, s.xOffset-shellHScrollStep)
			s.Bump()
			return true, nil
		}
	case "shift+right", "L":
		s.xOffset = min(s.xOffset+shellHScrollStep, max(s.maxLineWidth, s.xOffset))
		s.Bump()
		return true, nil
	}
	return false, nil
}

// ScrollHorizontal adjusts the horizontal scroll offset by delta columns.
func (s *ShellItem) ScrollHorizontal(delta int) {
	s.xOffset = max(0, s.xOffset+delta)
	if s.maxLineWidth > 0 {
		s.xOffset = min(s.xOffset, s.maxLineWidth)
	}
	s.Bump()
}

// ToggleExpanded toggles the expanded state and invalidates the cache.
func (s *ShellItem) ToggleExpanded() bool {
	s.expandedContent = !s.expandedContent
	s.Bump()
	return s.expandedContent
}

func (s *ShellItem) RawRender(width int) string {
	cappedWidth := cappedMessageWidth(width)

	cmd := strings.ReplaceAll(s.command, "\n", " ")
	cmd = strings.ReplaceAll(cmd, "\t", "    ")

	var prompt string
	if s.focused {
		prompt = s.sty.Messages.ShellPrompt.Render("$")
	} else {
		prompt = s.sty.Messages.ShellPromptBlurred.Render("$")
	}

	highlighted := s.sty.Messages.ShellCommand.Render(cmd)
	header := prompt + " " + highlighted

	if s.pending {
		if s.output.Len() == 0 {
			// Nothing streamed yet: show the spinner under the header.
			return header + "\n" + s.anim.Render()
		}
	} else if s.exitCode != 0 {
		header += " " + s.sty.Messages.ShellExitCode.Render(fmt.Sprintf("(exit %d)", s.exitCode))
	}

	if s.output.Len() == 0 {
		return header
	}

	// Remap raw ANSI 16-color codes onto legible Charmtone colors so
	// dark terminal defaults don't render illegibly on Sennit's
	// background.
	// Strip trailing whitespace and bare ANSI resets before remapping.
	// Programs like `task` emit "\x1b[0m\n" after their last line of
	// output; trimming only "\n" misses these because the reset bytes
	// sit between the content and the newline.
	raw := trimShellOutput(s.output.String())
	if raw == "" {
		return header
	}

	// Count lines from the trimmed output directly so truncation
	// logic cannot drift from the trimming loop above.
	totalLines := strings.Count(raw, "\n") + 1
	truncatedCount := 0
	if !s.expandedContent && totalLines > shellMaxCollapsedLines {
		truncatedCount = totalLines - shellMaxCollapsedLines
		if s.pending {
			raw = lastLines(raw, shellMaxCollapsedLines)
		} else {
			raw = firstLines(raw, shellMaxCollapsedLines)
		}
	}

	raw = common.StripCursorControl(raw)
	output := common.RemapANSI16(raw, s.sty.ANSI)
	lines := strings.Split(output, "\n")

	// Compute max line width for scroll clamping.
	maxW := 0
	outputStyle := s.sty.Messages.ShellOutput
	truncationStyle := s.sty.Messages.ShellTruncation
	if s.hovered {
		outputStyle = s.sty.Messages.ShellOutputHover
		truncationStyle = s.sty.Messages.ShellTruncationHover
	}
	for _, ln := range lines {
		w := ansi.StringWidth(ln)
		if w > maxW {
			maxW = w
		}
	}
	s.maxLineWidth = max(0, maxW-cappedWidth)

	var body strings.Builder

	// When streaming, hidden lines are above the visible tail, so show
	// the "more lines" notice before the output.
	if truncatedCount > 0 && s.pending {
		truncationStyle := s.sty.Messages.ShellTruncation
		if s.hovered {
			truncationStyle = s.sty.Messages.ShellTruncationHover.Width(cappedWidth)
		}
		body.WriteString(truncationStyle.Render(fmt.Sprintf("… %d earlier lines", truncatedCount)))
		body.WriteString("\n")
	}

	for _, ln := range lines {
		scrolled := ansi.GraphemeWidth.Cut(ln, s.xOffset, len(ln))
		// The leading "…" marking a horizontally-scrolled line must be
		// carved out of cappedWidth *before* truncating, not prepended
		// after — otherwise the line ends up a column wider than the
		// area once both the leading and ansi.Truncate's own trailing
		// "…" are in play.
		truncWidth := cappedWidth
		leading := s.xOffset > 0 && strings.TrimSpace(scrolled) != ""
		if leading {
			truncWidth = max(0, cappedWidth-ansi.StringWidth("…"))
		}
		truncated := ansi.Truncate(scrolled, truncWidth, "…")
		if leading {
			truncated = "…" + truncated
		}
		if s.hovered {
			outputStyle = outputStyle.Width(cappedWidth)
		}
		body.WriteString(outputStyle.Render(truncated))
		body.WriteString("\n")
	}

	// When finished, hidden lines are below, so show the notice after.
	if truncatedCount > 0 && !s.pending && !s.expandedContent {
		if s.hovered {
			truncationStyle = truncationStyle.Width(cappedWidth)
		}
		body.WriteString(truncationStyle.Render(fmt.Sprintf("… %d more lines", truncatedCount)))
		return header + "\n" + body.String()
	}

	result := header + "\n" + strings.TrimRight(body.String(), "\n")

	// While streaming, keep the spinner pinned below the latest output.
	if s.pending {
		result += "\n" + s.anim.Render()
	}

	return result
}

func firstLines(s string, count int) string {
	if count <= 0 {
		return ""
	}
	end := 0
	for i := range count {
		next := strings.IndexByte(s[end:], '\n')
		if next < 0 {
			return s
		}
		end += next
		if i == count-1 {
			return s[:end]
		}
		end++
	}
	return s
}

func lastLines(s string, count int) string {
	if count <= 0 {
		return ""
	}
	start := len(s)
	for range count {
		previous := strings.LastIndexByte(s[:start], '\n')
		if previous < 0 {
			return s
		}
		start = previous
	}
	return s[start+1:]
}
