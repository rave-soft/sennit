package diffview

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/aymanbagabas/go-udiff"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/ansiext"
	"github.com/rave-soft/sennit/internal/ui/xchroma"
	"github.com/zeebo/xxh3"
)

const (
	leadingSymbolsSize = 2
	lineNumPadding     = 1
)

type file struct {
	path    string
	content string
	// raw is the untouched content passed to Before/After. content gets
	// overwritten in place by normalizeLineEndings/replaceTabs on every
	// String() call; raw lets that reset from the original text each
	// time instead of re-processing already-processed content (see the
	// String() reset and replaceTabs' doc comment).
	raw string
}

type layout int

const (
	layoutUnified layout = iota + 1
	layoutSplit
)

// DiffView represents a view for displaying differences between two files.
type DiffView struct {
	layout          layout
	before          file
	after           file
	fileName        string
	contextLines    int
	lineNumbers     bool
	height          int
	width           int
	xOffset         int
	yOffset         int
	wrapLines       bool
	infiniteYScroll bool
	style           Style
	tabWidth        int
	chromaStyle     *chroma.Style

	isComputed bool
	err        error
	unified    udiff.UnifiedDiff
	edits      []udiff.Edit

	splitHunks []splitHunk

	totalLines      int
	codeWidth       int
	fullCodeWidth   int  // with leading symbols
	extraColOnAfter bool // add extra column on after panel
	beforeNumDigits int
	afterNumDigits  int

	// Cache lexer to avoid expensive file pattern matching on every line
	cachedLexer chroma.Lexer

	// Cache highlighted lines to avoid re-highlighting the same content
	// Key: hash of (content + background color), Value: highlighted string
	syntaxCache map[string]string
}

// New creates a new DiffView with default settings.
func New() *DiffView {
	dv := &DiffView{
		layout:       layoutUnified,
		contextLines: udiff.DefaultContextLines,
		lineNumbers:  true,
		tabWidth:     8,
		syntaxCache:  make(map[string]string),
	}
	dv.style = DefaultDarkStyle()
	return dv
}

// Unified sets the layout of the DiffView to unified.
func (dv *DiffView) Unified() *DiffView {
	dv.layout = layoutUnified
	return dv
}

// Split sets the layout of the DiffView to split (side-by-side).
func (dv *DiffView) Split() *DiffView {
	dv.layout = layoutSplit
	return dv
}

// Before sets the "before" file for the DiffView.
func (dv *DiffView) Before(path, content string) *DiffView {
	dv.before = file{path: path, content: content, raw: content}
	// Clear caches when content changes
	dv.clearCaches()
	return dv
}

// After sets the "after" file for the DiffView.
func (dv *DiffView) After(path, content string) *DiffView {
	dv.after = file{path: path, content: content, raw: content}
	// Clear caches when content changes
	dv.clearCaches()
	return dv
}

// clearCaches clears all caches when content or major settings change.
func (dv *DiffView) clearCaches() {
	dv.cachedLexer = nil
	dv.clearSyntaxCache()
	dv.isComputed = false
}

// Style sets the style for the DiffView.
func (dv *DiffView) Style(style Style) *DiffView {
	dv.style = style
	return dv
}

// Width sets the width of the DiffView.
func (dv *DiffView) Width(width int) *DiffView {
	dv.width = width
	return dv
}

// XOffset sets the horizontal offset for the DiffView. It is ignored while
// WrapLines is on: a wrapped line has no off-screen columns for the offset
// to reveal, so the wrapped renderers never consult xOffset. Callers that
// want horizontal scrolling must disable wrapping (see WrapLines).
func (dv *DiffView) XOffset(xOffset int) *DiffView {
	dv.xOffset = xOffset
	return dv
}

// WrapLines sets whether long code lines are wrapped to the available width.
// Continuation rows omit line numbers and diff symbols. Wrapping and
// XOffset are mutually exclusive: while WrapLines is true, XOffset has no
// effect, since every column of a wrapped line is already visible.
func (dv *DiffView) WrapLines(wrapLines bool) *DiffView {
	dv.wrapLines = wrapLines
	return dv
}

// TabWidth sets the tab width. Only relevant for code that contains tabs, like
// Go code.
func (dv *DiffView) TabWidth(tabWidth int) *DiffView {
	dv.tabWidth = tabWidth
	// See ContextLines: without this, a String() call already having run
	// once means replaceTabs re-runs against content whose tabs were
	// already expanded (and are therefore gone), so a later TabWidth
	// change would have nothing left to expand.
	dv.clearCaches()
	return dv
}

// ChromaStyle sets the chroma style for syntax highlighting.
// If nil, no syntax highlighting will be applied.
func (dv *DiffView) ChromaStyle(style *chroma.Style) *DiffView {
	dv.chromaStyle = style
	// Clear syntax cache when style changes since highlighting will be different
	dv.clearSyntaxCache()
	return dv
}

// clearSyntaxCache clears the syntax highlighting cache.
func (dv *DiffView) clearSyntaxCache() {
	if dv.syntaxCache != nil {
		// Clear the map but keep it allocated
		for k := range dv.syntaxCache {
			delete(dv.syntaxCache, k)
		}
	}
}

// String returns the string representation of the DiffView.
func (dv *DiffView) String() string {
	// Reset from the untouched raw content before normalizing/expanding
	// again: normalizeLineEndings and replaceTabs both mutate
	// dv.before/after.content in place, so without this a second
	// String() call (e.g. after a TabWidth() change) would run against
	// content whose tabs and CRLFs were already processed away on the
	// first call.
	dv.before.content = dv.before.raw
	dv.after.content = dv.after.raw
	dv.normalizeLineEndings()
	dv.replaceTabs()
	if err := dv.computeDiff(); err != nil {
		return err.Error()
	}
	dv.convertDiffToSplit()
	dv.adjustStyles()
	dv.detectNumDigits()
	if dv.width <= 0 {
		dv.detectCodeWidth()
	} else {
		dv.resizeCodeWidth()
	}
	dv.detectTotalLines()
	if dv.wrapLines {
		dv.detectWrappedTotalLines()
	}
	dv.preventInfiniteYScroll()

	style := lipgloss.NewStyle()
	if dv.width > 0 {
		style = style.MaxWidth(dv.width)
	}
	if dv.height > 0 {
		style = style.MaxHeight(dv.height)
	}

	switch dv.layout {
	case layoutUnified:
		if dv.wrapLines {
			return style.Render(strings.TrimSuffix(dv.renderWrappedUnified(), "\n"))
		}
		return style.Render(strings.TrimSuffix(dv.renderUnified(), "\n"))
	case layoutSplit:
		if dv.wrapLines {
			return style.Render(strings.TrimSuffix(dv.renderWrappedSplit(), "\n"))
		}
		return style.Render(strings.TrimSuffix(dv.renderSplit(), "\n"))
	default:
		panic("unknown diffview layout")
	}
}

// detectWrappedTotalLines accounts for code lines that occupy multiple visual
// rows when wrapping is enabled.
func (dv *DiffView) detectWrappedTotalLines() {
	for _, h := range dv.unified.Hunks {
		for _, l := range h.Lines {
			dv.totalLines += len(dv.wrapCode(l.Content, dv.codeWidth)) - 1
		}
	}

	if dv.layout != layoutSplit {
		return
	}

	// Split rows must be as tall as their tallest pane, rather than the sum of
	// both panes. Recalculate the code-row portion accordingly.
	dv.detectTotalLines()
	for _, h := range dv.splitHunks {
		for _, l := range h.lines {
			beforeRows, afterRows := 1, 1
			if l.before != nil {
				beforeRows = len(dv.wrapCode(l.before.Content, dv.codeWidth))
			}
			if l.after != nil {
				afterRows = len(dv.wrapCode(l.after.Content, dv.codeWidth+btoi(dv.extraColOnAfter)))
			}
			dv.totalLines += max(beforeRows, afterRows) - 1
		}
	}
}

// normalizeLineEndings ensures the file contents use Unix-style line endings.
func (dv *DiffView) normalizeLineEndings() {
	dv.before.content = strings.ReplaceAll(dv.before.content, "\r\n", "\n")
	dv.after.content = strings.ReplaceAll(dv.after.content, "\r\n", "\n")
}

// replaceTabs replaces tabs in the before and after file contents with spaces
// according to the specified tab width.
func (dv *DiffView) replaceTabs() {
	spaces := strings.Repeat(" ", dv.tabWidth)
	dv.before.content = strings.ReplaceAll(dv.before.content, "\t", spaces)
	dv.after.content = strings.ReplaceAll(dv.after.content, "\t", spaces)
}

// computeDiff computes the differences between the "before" and "after" files.
func (dv *DiffView) computeDiff() error {
	if dv.isComputed {
		return dv.err
	}
	dv.isComputed = true
	dv.edits = udiff.Lines(
		dv.before.content,
		dv.after.content,
	)
	dv.unified, dv.err = udiff.ToUnifiedDiff(
		dv.before.path,
		dv.after.path,
		dv.before.content,
		dv.edits,
		dv.contextLines,
	)
	return dv.err
}

// convertDiffToSplit converts the unified diff to a split diff if the layout is
// set to split.
func (dv *DiffView) convertDiffToSplit() {
	if dv.layout != layoutSplit {
		return
	}

	dv.splitHunks = make([]splitHunk, len(dv.unified.Hunks))
	for i, h := range dv.unified.Hunks {
		dv.splitHunks[i] = hunkToSplit(h)
	}
}

// adjustStyles adjusts adds padding and alignment to the styles.
func (dv *DiffView) adjustStyles() {
	setPadding := func(s lipgloss.Style) lipgloss.Style {
		return s.Padding(0, lineNumPadding).Align(lipgloss.Right)
	}
	dv.style.MissingLine.LineNumber = setPadding(dv.style.MissingLine.LineNumber)
	dv.style.DividerLine.LineNumber = setPadding(dv.style.DividerLine.LineNumber)
	dv.style.EqualLine.LineNumber = setPadding(dv.style.EqualLine.LineNumber)
	dv.style.InsertLine.LineNumber = setPadding(dv.style.InsertLine.LineNumber)
	dv.style.DeleteLine.LineNumber = setPadding(dv.style.DeleteLine.LineNumber)
	dv.style.Filename.LineNumber = setPadding(dv.style.Filename.LineNumber)
}

// detectNumDigits calculates the maximum number of digits needed for before and
// after line numbers.
func (dv *DiffView) detectNumDigits() {
	dv.beforeNumDigits = 0
	dv.afterNumDigits = 0

	for _, h := range dv.unified.Hunks {
		dv.beforeNumDigits = max(dv.beforeNumDigits, len(strconv.Itoa(h.FromLine+len(h.Lines))))
		dv.afterNumDigits = max(dv.afterNumDigits, len(strconv.Itoa(h.ToLine+len(h.Lines))))
	}
}

func (dv *DiffView) detectTotalLines() {
	dv.totalLines = 0

	if dv.fileName != "" {
		dv.totalLines++
	}

	switch dv.layout {
	case layoutUnified:
		for _, h := range dv.unified.Hunks {
			dv.totalLines += 1 + len(h.Lines)
		}
	case layoutSplit:
		for _, h := range dv.splitHunks {
			dv.totalLines += 1 + len(h.lines)
		}
	}
}

func (dv *DiffView) preventInfiniteYScroll() {
	if dv.infiniteYScroll {
		return
	}

	// clamp yOffset to prevent scrolling beyond the last line
	if dv.height > 0 {
		maxYOffset := max(0, dv.totalLines-dv.height)
		dv.yOffset = min(dv.yOffset, maxYOffset)
	} else {
		// if no height limit, ensure yOffset doesn't exceed total lines
		dv.yOffset = min(dv.yOffset, max(0, dv.totalLines-1))
	}
	dv.yOffset = max(0, dv.yOffset) // ensure yOffset is not negative
}

// detectCodeWidth calculates the maximum width of code lines in the diff view.
func (dv *DiffView) detectCodeWidth() {
	switch dv.layout {
	case layoutUnified:
		dv.detectUnifiedCodeWidth()
	case layoutSplit:
		dv.detectSplitCodeWidth()
	}
	dv.fullCodeWidth = dv.codeWidth + leadingSymbolsSize
}

// detectUnifiedCodeWidth calculates the maximum width of code lines in a
// unified diff.
func (dv *DiffView) detectUnifiedCodeWidth() {
	dv.codeWidth = 0

	for _, h := range dv.unified.Hunks {
		shownLines := ansi.StringWidth(dv.hunkLineFor(h))

		for _, l := range h.Lines {
			lineWidth := ansi.StringWidth(strings.TrimSuffix(l.Content, "\n")) + 1
			dv.codeWidth = max(dv.codeWidth, lineWidth, shownLines)
		}
	}
}

// detectSplitCodeWidth calculates the maximum width of code lines in a
// split diff.
func (dv *DiffView) detectSplitCodeWidth() {
	dv.codeWidth = 0

	for i, h := range dv.splitHunks {
		shownLines := ansi.StringWidth(dv.hunkLineFor(dv.unified.Hunks[i]))

		for _, l := range h.lines {
			if l.before != nil {
				codeWidth := ansi.StringWidth(strings.TrimSuffix(l.before.Content, "\n")) + 1
				dv.codeWidth = max(dv.codeWidth, codeWidth, shownLines)
			}
			if l.after != nil {
				codeWidth := ansi.StringWidth(strings.TrimSuffix(l.after.Content, "\n")) + 1
				dv.codeWidth = max(dv.codeWidth, codeWidth, shownLines)
			}
		}
	}
}

// resizeCodeWidth resizes the code width to fit within the specified width.
func (dv *DiffView) resizeCodeWidth() {
	fullNumWidth := dv.beforeNumDigits + dv.afterNumDigits
	fullNumWidth += lineNumPadding * 4 // left and right padding for both line numbers

	switch dv.layout {
	case layoutUnified:
		dv.codeWidth = dv.width - fullNumWidth - leadingSymbolsSize
	case layoutSplit:
		remainingWidth := dv.width - fullNumWidth - leadingSymbolsSize*2
		dv.codeWidth = remainingWidth / 2
		dv.extraColOnAfter = isOdd(remainingWidth)
	}

	dv.fullCodeWidth = dv.codeWidth + leadingSymbolsSize
}

// contentAndLeadingEllipsis renders one line's code content — highlighted,
// scrolled by dv.xOffset, then truncated to dv.codeWidth — and reports
// whether that scroll clipped visible text off the left edge (a leading
// ellipsis is owed). Shared by renderUnified and renderSplit, which used to
// each define this as an identical closure.
func (dv *DiffView) contentAndLeadingEllipsis(in string, ls LineStyle) (content string, leadingEllipsis bool) {
	content = strings.TrimSuffix(in, "\n")
	content = dv.highlightCode(content, ls.Code.GetBackground())
	content = ansi.GraphemeWidth.Cut(content, dv.xOffset, len(content))
	content = truncateCode(content, dv.codeWidth, ls)
	leadingEllipsis = dv.xOffset > 0 && strings.TrimSpace(content) != ""
	return content, leadingEllipsis
}

// unifiedHeaderLine renders one header row for the unified layout: fixed
// "…" line-number placeholders (when line numbers are shown) followed by
// text styled with sty and truncated to dv.fullCodeWidth. Shared by the
// filename row (first hunk only) and the hunk divider row (every hunk), in
// both renderUnified and renderWrappedUnified.
func (dv *DiffView) unifiedHeaderLine(sty LineStyle, text string) string {
	var row strings.Builder
	if dv.lineNumbers {
		row.WriteString(sty.LineNumber.Render(pad("…", dv.beforeNumDigits)))
		row.WriteString(sty.LineNumber.Render(pad("…", dv.afterNumDigits)))
	}
	row.WriteString(sty.Code.Width(dv.fullCodeWidth).Render(ansi.Truncate(text, dv.fullCodeWidth, "…")))
	return row.String()
}

// splitHeaderLine is unifiedHeaderLine for the split (side-by-side)
// layout: the header spans only the "before" column, followed by the
// "after" column's own line-number placeholder and a blank filler cell so
// the row's total width still matches a normal split line. Shared by the
// filename row (first hunk only) and the hunk divider row (every hunk), in
// both renderSplit and renderWrappedSplit.
func (dv *DiffView) splitHeaderLine(sty LineStyle, text string) string {
	var row strings.Builder
	if dv.lineNumbers {
		row.WriteString(sty.LineNumber.Render(pad("…", dv.beforeNumDigits)))
	}
	row.WriteString(sty.Code.Width(dv.fullCodeWidth).Render(ansi.Truncate(text, dv.fullCodeWidth, "…")))
	if dv.lineNumbers {
		row.WriteString(sty.LineNumber.Render(pad("…", dv.afterNumDigits)))
	}
	row.WriteString(sty.Code.Width(dv.fullCodeWidth + btoi(dv.extraColOnAfter)).Render(" "))
	return row.String()
}

// renderUnified renders the unified diff view as a string.
func (dv *DiffView) renderUnified() string {
	var b strings.Builder

	fullContentStyle := lipgloss.NewStyle().MaxWidth(dv.fullCodeWidth)
	printedLines := -dv.yOffset
	shouldWrite := func() bool { return printedLines >= 0 }

outer:
	for i, h := range dv.unified.Hunks {
		// Render file name header before the first hunk.
		if i == 0 && dv.fileName != "" {
			if shouldWrite() {
				b.WriteString(dv.unifiedHeaderLine(dv.style.Filename, "  "+dv.fileName))
				b.WriteString("\n")
			}
			printedLines++
		}
		if shouldWrite() {
			b.WriteString(dv.unifiedHeaderLine(dv.style.DividerLine, dv.hunkLineFor(h)))
			b.WriteString("\n")
		}
		printedLines++

		beforeLine := h.FromLine
		afterLine := h.ToLine

		for j, l := range h.Lines {
			// print ellipis if we don't have enough space to print the rest of the diff
			hasReachedHeight := dv.height > 0 && printedLines+1 == dv.height
			isLastHunk := i+1 == len(dv.unified.Hunks)
			isLastLine := j+1 == len(h.Lines)
			if hasReachedHeight && (!isLastHunk || !isLastLine) {
				if shouldWrite() {
					ls := dv.lineStyleForType(l.Kind)
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad("…", dv.beforeNumDigits)))
						b.WriteString(ls.LineNumber.Render(pad("…", dv.afterNumDigits)))
					}
					b.WriteString(fullContentStyle.Render(
						ls.Code.Width(dv.fullCodeWidth).Render("  …"),
					))
					b.WriteRune('\n')
				}
				break outer
			}

			switch l.Kind {
			case udiff.Equal:
				if shouldWrite() {
					ls := dv.style.EqualLine
					content, leadingEllipsis := dv.contentAndLeadingEllipsis(l.Content, ls)
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(beforeLine, dv.beforeNumDigits)))
						b.WriteString(ls.LineNumber.Render(pad(afterLine, dv.afterNumDigits)))
					}
					b.WriteString(fullContentStyle.Render(
						ls.Code.Width(dv.fullCodeWidth).Render(ternary(leadingEllipsis, " …", "  ") + content),
					))
				}
				beforeLine++
				afterLine++
			case udiff.Insert:
				if shouldWrite() {
					ls := dv.style.InsertLine
					content, leadingEllipsis := dv.contentAndLeadingEllipsis(l.Content, ls)
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(" ", dv.beforeNumDigits)))
						b.WriteString(ls.LineNumber.Render(pad(afterLine, dv.afterNumDigits)))
					}
					b.WriteString(fullContentStyle.Render(
						ls.Symbol.Render(ternary(leadingEllipsis, "+…", "+ ")) +
							ls.Code.Width(dv.codeWidth).Render(content),
					))
				}
				afterLine++
			case udiff.Delete:
				if shouldWrite() {
					ls := dv.style.DeleteLine
					content, leadingEllipsis := dv.contentAndLeadingEllipsis(l.Content, ls)
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(beforeLine, dv.beforeNumDigits)))
						b.WriteString(ls.LineNumber.Render(pad(" ", dv.afterNumDigits)))
					}
					b.WriteString(fullContentStyle.Render(
						ls.Symbol.Render(ternary(leadingEllipsis, "-…", "- ")) +
							ls.Code.Width(dv.codeWidth).Render(content),
					))
				}
				beforeLine++
			}
			if shouldWrite() {
				b.WriteRune('\n')
			}

			printedLines++
		}
	}

	return b.String()
}

// truncateCode truncates syntax-highlighted content to width, appending an
// ellipsis when it doesn't fit.
//
// This can't just be ansi.Truncate(content, width, "…"): that pastes the
// ellipsis in using whatever SGR state happens to be active right at the
// cut point, and xchroma.Formatter emits some tokens — notably runs of
// whitespace with no chroma style entry — as completely bare, unstyled
// text. Landing the cut inside or right after one of those runs leaves the
// ellipsis with no color at all instead of matching the line. Cutting the
// body separately and rendering the ellipsis with the line's own style
// sidesteps whatever token it would have landed on.
func truncateCode(content string, width int, ls LineStyle) string {
	if ansi.StringWidth(content) <= width {
		return content
	}
	if width < 1 {
		// No room even for the ellipsis alone (mirrors ansi.Truncate,
		// which drops the tail entirely once its own width exceeds
		// the budget).
		return ""
	}
	return ansi.Truncate(content, width-1, "") + ls.Code.Render("…")
}

func (dv *DiffView) wrapCode(content string, width int) []string {
	content = strings.TrimSuffix(content, "\n")
	if width < 1 {
		width = 1
	}
	wrapped := ansi.Hardwrap(content, width, true)
	return strings.Split(wrapped, "\n")
}

func (dv *DiffView) visibleRows(rows []string) []string {
	start := min(dv.yOffset, len(rows))
	rows = rows[start:]
	if dv.height > 0 {
		rows = rows[:min(dv.height, len(rows))]
	}
	return rows
}

func (dv *DiffView) renderWrappedUnified() string {
	var rows []string
	for i, h := range dv.unified.Hunks {
		if i == 0 && dv.fileName != "" {
			rows = append(rows, dv.unifiedHeaderLine(dv.style.Filename, "  "+dv.fileName))
		}
		rows = append(rows, dv.unifiedHeaderLine(dv.style.DividerLine, dv.hunkLineFor(h)))

		beforeLine, afterLine := h.FromLine, h.ToLine
		for _, l := range h.Lines {
			ls := dv.lineStyleForType(l.Kind)
			parts := dv.wrapCode(dv.highlightCode(strings.TrimSuffix(l.Content, "\n"), ls.Code.GetBackground()), dv.codeWidth)
			for j, part := range parts {
				row := ""
				if dv.lineNumbers {
					before, after := " ", " "
					if j == 0 {
						switch l.Kind {
						case udiff.Equal:
							before, after = strconv.Itoa(beforeLine), strconv.Itoa(afterLine)
						case udiff.Insert:
							after = strconv.Itoa(afterLine)
						case udiff.Delete:
							before = strconv.Itoa(beforeLine)
						}
					}
					row += ls.LineNumber.Render(pad(before, dv.beforeNumDigits))
					row += ls.LineNumber.Render(pad(after, dv.afterNumDigits))
				}
				prefix := "  "
				if j == 0 {
					switch l.Kind {
					case udiff.Insert:
						prefix = ls.Symbol.Render("+ ")
					case udiff.Delete:
						prefix = ls.Symbol.Render("- ")
					}
				}
				rows = append(rows, row+ls.Code.Width(dv.fullCodeWidth).Render(prefix+part))
			}
			switch l.Kind {
			case udiff.Equal:
				beforeLine++
				afterLine++
			case udiff.Insert:
				afterLine++
			case udiff.Delete:
				beforeLine++
			}
		}
	}
	return strings.Join(dv.visibleRows(rows), "\n")
}

func (dv *DiffView) renderWrappedSplit() string {
	var rows []string
	for i, h := range dv.splitHunks {
		if i == 0 && dv.fileName != "" {
			rows = append(rows, dv.splitHeaderLine(dv.style.Filename, "  "+dv.fileName))
		}
		rows = append(rows, dv.splitHeaderLine(dv.style.DividerLine, dv.hunkLineFor(dv.unified.Hunks[i])))

		beforeLine, afterLine := h.fromLine, h.toLine
		for _, l := range h.lines {
			before := dv.wrappedSplitPane(l.before, beforeLine, dv.beforeNumDigits, dv.fullCodeWidth)
			after := dv.wrappedSplitPane(l.after, afterLine, dv.afterNumDigits, dv.fullCodeWidth+btoi(dv.extraColOnAfter))
			count := max(len(before), len(after))
			for j := range count {
				left, right := dv.emptySplitPane(dv.beforeNumDigits, dv.fullCodeWidth), dv.emptySplitPane(dv.afterNumDigits, dv.fullCodeWidth+btoi(dv.extraColOnAfter))
				if j < len(before) {
					left = before[j]
				}
				if j < len(after) {
					right = after[j]
				}
				rows = append(rows, left+right)
			}
			if l.before != nil {
				beforeLine++
			}
			if l.after != nil {
				afterLine++
			}
		}
	}
	return strings.Join(dv.visibleRows(rows), "\n")
}

func (dv *DiffView) emptySplitPane(numDigits, fullWidth int) string {
	row := ""
	if dv.lineNumbers {
		row = dv.style.MissingLine.LineNumber.Render(pad(" ", numDigits))
	}
	return row + dv.style.MissingLine.Code.Width(fullWidth).Render("  ")
}

func (dv *DiffView) wrappedSplitPane(line *udiff.Line, lineNumber, numDigits, fullWidth int) []string {
	if line == nil {
		return []string{dv.emptySplitPane(numDigits, fullWidth)}
	}
	ls := dv.lineStyleForType(line.Kind)
	codeWidth := fullWidth - leadingSymbolsSize
	parts := dv.wrapCode(dv.highlightCode(strings.TrimSuffix(line.Content, "\n"), ls.Code.GetBackground()), codeWidth)
	rows := make([]string, len(parts))
	for i, part := range parts {
		row := ""
		if dv.lineNumbers {
			number := " "
			if i == 0 {
				number = strconv.Itoa(lineNumber)
			}
			row = ls.LineNumber.Render(pad(number, numDigits))
		}
		prefix := "  "
		if i == 0 {
			switch line.Kind {
			case udiff.Insert:
				prefix = ls.Symbol.Render("+ ")
			case udiff.Delete:
				prefix = ls.Symbol.Render("- ")
			}
		}
		rows[i] = row + ls.Code.Width(fullWidth).Render(prefix+part)
	}
	return rows
}

// renderSplit renders the split (side-by-side) diff view as a string.
func (dv *DiffView) renderSplit() string {
	var b strings.Builder

	beforeFullContentStyle := lipgloss.NewStyle().MaxWidth(dv.fullCodeWidth)
	afterFullContentStyle := lipgloss.NewStyle().MaxWidth(dv.fullCodeWidth + btoi(dv.extraColOnAfter))
	printedLines := -dv.yOffset
	shouldWrite := func() bool { return printedLines >= 0 }

outer:
	for i, h := range dv.splitHunks {
		// Render file name header before the first hunk.
		if i == 0 && dv.fileName != "" {
			if shouldWrite() {
				b.WriteString(dv.splitHeaderLine(dv.style.Filename, "  "+dv.fileName))
				b.WriteRune('\n')
			}
			printedLines++
		}
		if shouldWrite() {
			b.WriteString(dv.splitHeaderLine(dv.style.DividerLine, dv.hunkLineFor(dv.unified.Hunks[i])))
			b.WriteRune('\n')
		}
		printedLines++

		beforeLine := h.fromLine
		afterLine := h.toLine

		for j, l := range h.lines {
			// print ellipis if we don't have enough space to print the rest of the diff
			hasReachedHeight := dv.height > 0 && printedLines+1 == dv.height
			isLastHunk := i+1 == len(dv.unified.Hunks)
			isLastLine := j+1 == len(h.lines)
			if hasReachedHeight && (!isLastHunk || !isLastLine) {
				if shouldWrite() {
					ls := dv.style.MissingLine
					if l.before != nil {
						ls = dv.lineStyleForType(l.before.Kind)
					}
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad("…", dv.beforeNumDigits)))
					}
					b.WriteString(beforeFullContentStyle.Render(
						ls.Code.Width(dv.fullCodeWidth).Render("  …"),
					))
					ls = dv.style.MissingLine
					if l.after != nil {
						ls = dv.lineStyleForType(l.after.Kind)
					}
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad("…", dv.afterNumDigits)))
					}
					b.WriteString(afterFullContentStyle.Render(
						ls.Code.Width(dv.fullCodeWidth).Render("  …"),
					))
					b.WriteRune('\n')
				}
				break outer
			}

			switch {
			case l.before == nil:
				if shouldWrite() {
					ls := dv.style.MissingLine
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(" ", dv.beforeNumDigits)))
					}
					b.WriteString(beforeFullContentStyle.Render(
						ls.Code.Width(dv.fullCodeWidth).Render("  "),
					))
				}
			case l.before.Kind == udiff.Equal:
				if shouldWrite() {
					ls := dv.style.EqualLine
					content, leadingEllipsis := dv.contentAndLeadingEllipsis(l.before.Content, ls)
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(beforeLine, dv.beforeNumDigits)))
					}
					b.WriteString(beforeFullContentStyle.Render(
						ls.Code.Width(dv.fullCodeWidth).Render(ternary(leadingEllipsis, " …", "  ") + content),
					))
				}
				beforeLine++
			case l.before.Kind == udiff.Delete:
				if shouldWrite() {
					ls := dv.style.DeleteLine
					content, leadingEllipsis := dv.contentAndLeadingEllipsis(l.before.Content, ls)
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(beforeLine, dv.beforeNumDigits)))
					}
					b.WriteString(beforeFullContentStyle.Render(
						ls.Symbol.Render(ternary(leadingEllipsis, "-…", "- ")) +
							ls.Code.Width(dv.codeWidth).Render(content),
					))
				}
				beforeLine++
			}

			switch {
			case l.after == nil:
				if shouldWrite() {
					ls := dv.style.MissingLine
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(" ", dv.afterNumDigits)))
					}
					b.WriteString(afterFullContentStyle.Render(
						ls.Code.Width(dv.fullCodeWidth + btoi(dv.extraColOnAfter)).Render("  "),
					))
				}
			case l.after.Kind == udiff.Equal:
				if shouldWrite() {
					ls := dv.style.EqualLine
					content, leadingEllipsis := dv.contentAndLeadingEllipsis(l.after.Content, ls)
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(afterLine, dv.afterNumDigits)))
					}
					b.WriteString(afterFullContentStyle.Render(
						ls.Code.Width(dv.fullCodeWidth + btoi(dv.extraColOnAfter)).Render(ternary(leadingEllipsis, " …", "  ") + content),
					))
				}
				afterLine++
			case l.after.Kind == udiff.Insert:
				if shouldWrite() {
					ls := dv.style.InsertLine
					content, leadingEllipsis := dv.contentAndLeadingEllipsis(l.after.Content, ls)
					if dv.lineNumbers {
						b.WriteString(ls.LineNumber.Render(pad(afterLine, dv.afterNumDigits)))
					}
					b.WriteString(afterFullContentStyle.Render(
						ls.Symbol.Render(ternary(leadingEllipsis, "+…", "+ ")) +
							ls.Code.Width(dv.codeWidth+btoi(dv.extraColOnAfter)).Render(content),
					))
				}
				afterLine++
			}

			if shouldWrite() {
				b.WriteRune('\n')
			}

			printedLines++
		}
	}

	return b.String()
}

// hunkLineFor formats the header line for a hunk in the unified diff view.
func (dv *DiffView) hunkLineFor(h *udiff.Hunk) string {
	beforeShownLines, afterShownLines := dv.hunkShownLines(h)

	return fmt.Sprintf(
		"  @@ -%d,%d +%d,%d @@ ",
		h.FromLine,
		beforeShownLines,
		h.ToLine,
		afterShownLines,
	)
}

// hunkShownLines calculates the number of lines shown in a hunk for both before
// and after versions.
func (dv *DiffView) hunkShownLines(h *udiff.Hunk) (before, after int) {
	for _, l := range h.Lines {
		switch l.Kind {
		case udiff.Equal:
			before++
			after++
		case udiff.Insert:
			after++
		case udiff.Delete:
			before++
		}
	}
	return before, after
}

func (dv *DiffView) lineStyleForType(t udiff.OpKind) LineStyle {
	switch t {
	case udiff.Equal:
		return dv.style.EqualLine
	case udiff.Insert:
		return dv.style.InsertLine
	case udiff.Delete:
		return dv.style.DeleteLine
	default:
		return dv.style.MissingLine
	}
}

func (dv *DiffView) highlightCode(source string, bgColor color.Color) string {
	if dv.chromaStyle == nil {
		return source
	}

	// Create cache key from content and background color
	cacheKey := dv.createSyntaxCacheKey(source, bgColor)

	// Check if we already have this highlighted
	if cached, exists := dv.syntaxCache[cacheKey]; exists {
		return cached
	}

	l := dv.getChromaLexer()
	f := dv.getChromaFormatter(bgColor)

	it, err := l.Tokenise(nil, source)
	if err != nil {
		return source
	}

	var b strings.Builder
	if err := f.Format(&b, dv.chromaStyle, it); err != nil {
		return source
	}

	result := b.String()

	// Cache the result for future use
	dv.syntaxCache[cacheKey] = result

	return result
}

// createSyntaxCacheKey creates a cache key from source content and background color.
// We use a simple hash to keep memory usage reasonable.
func (dv *DiffView) createSyntaxCacheKey(source string, bgColor color.Color) string {
	// Convert color to string representation
	r, g, b, a := bgColor.RGBA()
	colorStr := fmt.Sprintf("%d,%d,%d,%d", r, g, b, a)

	// Create a hash of the content + color to use as cache key
	h := xxh3.New()
	_, _ = h.Write([]byte(source))
	_, _ = h.Write([]byte(colorStr))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (dv *DiffView) getChromaLexer() chroma.Lexer {
	if dv.cachedLexer != nil {
		return dv.cachedLexer
	}

	// MatchLexer is memoized and already coalesced; the per-DiffView cache
	// still avoids repeat lookups within a single render.
	if l := xchroma.MatchLexer(dv.before.path); l != nil {
		dv.cachedLexer = l
		return dv.cachedLexer
	}
	l := lexers.Analyse(dv.before.content)
	if l == nil {
		l = lexers.Fallback
	}
	dv.cachedLexer = chroma.Coalesce(l)
	return dv.cachedLexer
}

func (dv *DiffView) getChromaFormatter(bgColor color.Color) chroma.Formatter {
	return xchroma.Formatter(bgColor, processChromaValue)
}

func processChromaValue(value string) string {
	value = strings.TrimRight(value, "\n")
	value = ansiext.Escape(value)
	return value
}
