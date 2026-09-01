package chat

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// assistantMessageTruncateFormat is the text shown when an assistant message is
// truncated in the collapsed state.
const assistantMessageTruncateFormat = "… (%d lines hidden) [click or space to expand]"

// assistantMessageTailWindowFormat is shown above a tail-windowed thinking
// block to advertise that earlier lines exist and that the user can
// promote the view to a full expansion. The promotion is wired through
// the existing ToggleExpanded path (click / space) — F5 deliberately
// does not add a new keybinding.
const assistantMessageTailWindowFormat = "… %d earlier lines hidden [click or space for full view]"

// maxCollapsedThinkingHeight defines the maximum height of the thinking
const maxCollapsedThinkingHeight = 10

// Default copy for a provider-refusal banner. The agent persists only
// the FinishReasonContentFilter reason; the TUI owns this text and
// fills it in when the finish part carries no message/details (the
// normal live path, and restored sessions). Kept here as the single
// source of truth so the render path and tests cannot drift apart.
const (
	refusalTagLabel = "REFUSED"
	refusalTitle    = "Model refused to continue"
	refusalDetails  = "The provider's safety classifier stopped this response before any usable content was produced. Rephrase the request, start a fresh session, or try a different model."
)

// maxExpandedThinkingTailLines is the F5 tail-window cap. When the user
// expands a thinking block whose post-glamour line count exceeds this
// threshold, only the last N lines are shown with an affordance line
// indicating how many earlier lines are hidden. Clicking / pressing
// space again promotes the view to a full expansion. The slice is
// taken AFTER glamour render (not before) so fenced code blocks,
// lists, and tables are not torn at arbitrary boundaries.
const maxExpandedThinkingTailLines = 200

// thinkingViewMode is the F5 three-state view machine for the thinking
// block. ToggleExpanded cycles
// collapsed → tail-window → full-expanded → collapsed, skipping the
// tail-window step when the rendered thinking fits within the cap so
// short blocks still toggle in two clicks.
type thinkingViewMode uint8

const (
	thinkingCollapsed thinkingViewMode = iota
	thinkingTailWindow
	thinkingFullExpanded
)

// assistantSection is a per-section render cache for AssistantMessageItem.
// Each section (thinking, content, error) carries its own keys so that
// streaming a section does not invalidate a different — often more
// expensive — section's cached render. srcHash is an FNV-64 of the
// section's source text; extra captures any other state that changes
// the rendered output (e.g. thinkingExpanded, the thinking footer
// inputs). valid disambiguates a real cache hit from the zero value
// when both source text and extras hash to zero. aux carries any
// per-section side data that the caller needs to recover on a hit
// (e.g. the thinking box height for click detection).
type assistantSection struct {
	width   int
	srcHash uint64
	extra   uint64
	out     string
	h       int
	aux     int
	valid   bool
}

// hit reports whether the cache entry matches the requested key.
func (s *assistantSection) hit(width int, srcHash, extra uint64) bool {
	return s.valid && s.width == width && s.srcHash == srcHash && s.extra == extra
}

// store records the rendered output under the given key.
func (s *assistantSection) store(width int, srcHash, extra uint64, out string, aux int) {
	s.width = width
	s.srcHash = srcHash
	s.extra = extra
	s.out = out
	s.h = lipgloss.Height(out)
	s.aux = aux
	s.valid = true
}

// reset drops the cached output.
func (s *assistantSection) reset() {
	*s = assistantSection{}
}

// fnv64 hashes a single string with FNV-64.
func fnv64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// countLines returns the number of lines in s (i.e. the number of
// newline-separated segments). Equivalent to len(strings.Split(s,
// "\n")) but allocates nothing. See upstream ticket CHARM-1785.
func countLines(s string) int {
	if s == "" {
		return 1
	}
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			n++
		}
	}
	return n
}

// tailLines returns the last n lines of s and the count of hidden
// (earlier) lines. totalLines is the pre-computed line count of s
// (from countLines). It finds the cut point with a bounded backward
// scan so the cost is O(n) in the number of kept lines, not O(L)
// in the total document length. See upstream ticket CHARM-1785.
func tailLines(s string, n, totalLines int) (tail string, hidden int) {
	if n <= 0 {
		return "", totalLines
	}
	if totalLines <= n {
		return s, 0
	}
	// Find the nth newline from the end. The tail starts after it.
	count := 0
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			count++
			if count == n {
				return s[i+1:], totalLines - n
			}
		}
	}
	return s, 0
}

// fnvFields hashes a list of byte fields with length-prefix framing
// so that no concatenation collision can occur between distinct
// field tuples (a NUL inside one field cannot impersonate a
// boundary between two fields). Each field is preceded by its
// length encoded as 8 bytes little-endian.
func fnvFields(fields ...[]byte) uint64 {
	h := fnv.New64a()
	var lenBuf [8]byte
	for _, f := range fields {
		binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(f)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(f)
	}
	return h.Sum64()
}

// AssistantMessageItem represents an assistant message in the chat UI.
//
// This item includes thinking, and the content but does not include the tool calls.
type AssistantMessageItem struct {
	*list.Versioned
	*highlightableMessageItem
	*cachedMessageItem
	*focusableMessageItem

	message           *message.Message
	sty               *styles.Styles
	anim              *spin.Anim
	animLabel         string // last label pushed into anim; see renderSpinning
	thinkingViewMode  thinkingViewMode
	thinkingBoxHeight int // Tracks the rendered thinking box height for click detection.
	hovered           bool

	// Incremental FNV-64a hash of the thinking text. Avoids
	// re-hashing the entire accumulated text on every streaming
	// tick. thinkingHashSample holds a short prefix of the hashed
	// text so we can detect divergence (e.g. a user retry that
	// rewrites the thinking from scratch) without re-hashing the
	// whole thing. See upstream ticket CHARM-1785.
	thinkingHash       uint64
	thinkingHashLen    int
	thinkingHashSample string
	// thinkingHashFullRehashes counts calls to thinkingHashIncremental
	// that took the full re-hash branch (first call, or the text
	// shrank/diverged) rather than the incremental fast path. It has no
	// effect on behavior — it exists so tests can catch a regression
	// where the fast path silently stops being taken (e.g. thinkingKey
	// getting called more than once per render pass, which starves it of
	// the strict length growth it needs — see computeSectionKeys' doc
	// comment) without depending on timing.
	thinkingHashFullRehashes int

	// Per-section render caches. Splitting these out means content
	// streaming does not invalidate the (often expensive) thinking
	// render, and vice versa.
	thinkingSec assistantSection
	contentSec  assistantSection
	errorSec    assistantSection

	// streamingContent caches a "stable prefix" glamour render of
	// the assistant content body so each streaming flush only
	// re-renders the trailing partial. F8 of
	// docs/notes/2026-05-12-chat-rendering-perf.md. See
	// streaming_markdown.go for the full algorithm.
	streamingContent streamingMarkdown

	// streamingThinking applies the same stable-prefix caching to
	// the thinking/reasoning section. Without this, every streaming
	// delta forces a full glamour re-render of the entire accumulated
	// thinking text, which burns CPU and starves the terminal emulator
	// during long reasoning traces.
	streamingThinking streamingMarkdown

	// summaryRenderedLines is the height of the last expanded-summary
	// render, so the footer that closes it can be found by row index
	// from a mouse event - see summaryFooterLine. Zero when the item is
	// not an expanded summary.
	summaryRenderedLines int
	// summaryExpanded opens a summarize pass's output, which renders as
	// a single collapsed row otherwise. Meaningful only when the
	// message carries IsSummaryMessage; see assistant_summary.go.
	summaryExpanded bool
}

var _ Expandable = (*AssistantMessageItem)(nil)

// NewAssistantMessageItem creates a new AssistantMessageItem.
func NewAssistantMessageItem(sty *styles.Styles, message *message.Message) MessageItem {
	v := list.NewVersioned()
	a := &AssistantMessageItem{
		Versioned:                v,
		highlightableMessageItem: defaultHighlighter(sty, v),
		cachedMessageItem:        &cachedMessageItem{},
		focusableMessageItem:     newFocusableMessageItem(v),
		message:                  message,
		sty:                      sty,
	}

	a.anim = a.newAnim()
	return a
}

// newAnim builds the working animation from the item's current styles.
// Both construction and [AssistantMessageItem.Restyle] go through it so a
// rebuilt animation cannot drift from the original settings.
func (a *AssistantMessageItem) newAnim() *spin.Anim {
	return spin.New(spin.Settings{
		ID:          a.ID(),
		Size:        15,
		GradColorA:  a.sty.WorkingGradFromColor,
		GradColorB:  a.sty.WorkingGradToColor,
		LabelColor:  a.sty.WorkingLabelColor,
		CycleColors: true,
		Mode:        a.sty.WorkingSpinner,
		Suffix: func() string {
			return common.Elapsed(a.message.SessionID)
		},
		SuffixColor: a.sty.WorkingTimerColor,
	})
}

// Restyle implements [Restylable]: the working animation pre-renders its
// gradient from the palette it was built with, so it is rebuilt (and
// re-armed if it was running) rather than restyled in place.
func (a *AssistantMessageItem) Restyle() tea.Cmd {
	a.anim = a.newAnim()
	// The rebuilt animation has no label; forget which one it had so
	// renderSpinning pushes the current phase back into it.
	a.animLabel = ""
	a.Bump()
	return a.StartAnimation()
}

// StartAnimation starts the assistant message animation if it should be spinning.
func (a *AssistantMessageItem) StartAnimation() tea.Cmd {
	if !a.spinnerActive() {
		return nil
	}
	return a.anim.Start()
}

// Animate progresses the assistant message animation if it should be spinning.
func (a *AssistantMessageItem) Animate(msg spin.StepMsg) tea.Cmd {
	if !a.spinnerActive() {
		return nil
	}
	// Bump the F6 list-cache version so the next draw re-renders
	// this item: a spinner tick mutates anim's internal frame
	// counter, which changes the rendered output but is invisible
	// to the per-section content hashes. Without the bump the
	// list cache would serve the previously rendered frame
	// indefinitely and the spinner would appear frozen.
	a.Bump()
	return a.anim.Animate(msg)
}

// ID implements MessageItem.
func (a *AssistantMessageItem) ID() string {
	return a.message.ID
}

// RawRender implements [MessageItem]. It computes the section cache keys
// itself; renderRaw is the shared body so Render below can instead reuse
// the keys it already computed for its own cache check (see renderRaw's
// comment for why that matters).
func (a *AssistantMessageItem) RawRender(width int) string {
	return a.renderRaw(width, a.computeSectionKeys())
}

// renderRaw is RawRender's body, parameterized on already-computed section
// keys so a caller that needs them anyway (Render, below) doesn't force a
// second computation of thinkingKey/contentKey/errorKey per frame.
func (a *AssistantMessageItem) renderRaw(width int, keys assistantSectionKeys) string {
	cappedWidth := cappedMessageWidth(width)

	// A collapsed summary is a row, not a message: it never reaches
	// renderMessageContent, so none of its text — or its reasoning —
	// streams into the window. See assistant_summary.go.
	if a.summaryIsCollapsed() {
		return a.renderCollapsedSummary(cappedWidth)
	}

	var spinner string
	if a.isSpinning() {
		spinner = a.renderSpinning()
	}

	content, height := a.renderMessageContent(cappedWidth, keys)
	highlightedContent := a.renderHighlighted(content, cappedWidth, height)
	// An expanded summary keeps its header, so the row the person opened
	// is still there to close again and the text below it stays labelled
	// as a compaction rather than reading as an ordinary reply. The
	// header is unconditional: a summary that came back empty must still
	// leave a row to collapse, or expanding it would make it disappear.
	if a.isSummary() {
		header := a.renderSummaryHeader(cappedWidth)
		if highlightedContent == "" {
			highlightedContent = header
		} else {
			highlightedContent = header + "\n\n" + highlightedContent
		}
	}
	if spinner != "" {
		if highlightedContent != "" {
			highlightedContent += "\n\n"
		}
		highlightedContent += spinner
	}
	// The footer goes last, below the spinner included: it is the
	// control that closes the block, and a control belongs at the edge
	// of what it acts on. Recording the height here is what lets a
	// click find it - see summaryFooterLine.
	if a.isSummary() && a.summaryExpanded {
		highlightedContent += "\n" + a.renderSummaryFooter(cappedWidth)
		a.summaryRenderedLines = lipgloss.Height(highlightedContent)
	} else {
		a.summaryRenderedLines = 0
	}

	return highlightedContent
}

// Render implements MessageItem.
func (a *AssistantMessageItem) Render(width int) string {
	// XXX: Here, we're manually applying the focused/blurred styles because
	// using lipgloss.Render can degrade performance for long messages due to
	// it's wrapping logic.
	// We already know that the content is wrapped to the correct width in
	// RawRender, so we can just apply the styles directly to each line.
	//
	// The split + per-line prefix loop is O(L); cache the result keyed
	// by (width, focused, sectionsFingerprint) so steady-state Render
	// becomes a pointer return. The sectionsFingerprint folds in the
	// per-section srcHash/extra so that any sub-cache change
	// invalidates this prefix cache without requiring an explicit
	// drop. Bypass the cache while spinning (RawRender's spinner
	// suffix changes every animation frame) or while a highlight
	// range is active (selection drag).
	useCache := !a.isSpinning() && !a.isHighlighted()
	cappedWidth := cappedMessageWidth(width)
	// Computed once and threaded through both the cache-key check and (on
	// a miss) the actual render below — see renderRaw's comment.
	keys := a.computeSectionKeys()
	key := a.prefixCacheKey(cappedWidth, keys)
	return a.renderCachedPrefixed(width, key, useCache, func() string {
		prefix := a.sty.Messages.AssistantBlurred.Render()
		if a.focused {
			prefix = a.sty.Messages.AssistantFocused.Render()
		}
		return prefixLines(a.renderRaw(width, keys), prefix)
	})
}

// assistantSectionKeys bundles the (srcHash, extra) cache-key pair for each
// of the three renderable sections, computed once per render pass by
// computeSectionKeys. Threading this through renderRaw/renderMessageContent
// instead of having prefixCacheKey and cachedThinking/cachedContent/
// cachedError each call thinkingKey/contentKey/errorKey independently is
// what keeps thinkingHashIncremental's fast path honest: that hash is
// continued from saved state only when the text strictly grew since the
// last call, and calling it twice within the same frame (once for the
// cache check, once for the actual render) always failed that check on the
// second call — forcing a full re-hash every single tick regardless of the
// "incremental" bookkeeping.
type assistantSectionKeys struct {
	thinkSrc, thinkExtra     uint64
	contentSrc, contentExtra uint64
	errSrc, errExtra         uint64
}

// computeSectionKeys computes every section's cache key exactly once.
func (a *AssistantMessageItem) computeSectionKeys() assistantSectionKeys {
	thinkSrc, thinkExtra := a.thinkingKey()
	contentSrc, contentExtra := a.contentKey()
	errSrc, errExtra := a.errorKey()
	return assistantSectionKeys{
		thinkSrc: thinkSrc, thinkExtra: thinkExtra,
		contentSrc: contentSrc, contentExtra: contentExtra,
		errSrc: errSrc, errExtra: errExtra,
	}
}

// prefixCacheKey builds the F3 prefixed-render cache key. We pack the
// focus bit into bit 0 and a fingerprint of the section caches into
// the upper bits, so any change to a sub-section's source text or
// extras forces the prefix cache to miss without needing an explicit
// drop. cappedWidth is included so a cached prefix never survives a
// section-cache miss caused by a width change. The finish reason is
// folded in too because it controls the composition of
// renderMessageContent (e.g. appending the constant "Canceled"
// string) — that decision lives outside any section's own hash.
func (a *AssistantMessageItem) prefixCacheKey(cappedWidth int, keys assistantSectionKeys) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	writeU64 := func(v uint64) {
		for i := range 8 {
			buf[i] = byte(v >> (8 * i))
		}
		_, _ = h.Write(buf[:])
	}
	writeU64(uint64(cappedWidth))
	writeU64(keys.thinkSrc)
	writeU64(keys.thinkExtra)
	writeU64(keys.contentSrc)
	writeU64(keys.contentExtra)
	writeU64(keys.errSrc)
	writeU64(keys.errExtra)
	writeU64(a.compositionKey())
	fingerprint := h.Sum64()
	var focusBit uint64
	if a.focused {
		focusBit = 1
	}
	return (fingerprint &^ 1) | focusBit
}

// compositionKey hashes the inputs to renderMessageContent's structural
// decisions (which sections to include, whether to append the
// constant "Canceled" footer) so that flipping IsFinished or the
// finish reason invalidates the prefix cache even when no section's
// own source text changed.
func (a *AssistantMessageItem) compositionKey() uint64 {
	var finishedFlag byte
	var reason string
	if a.message.IsFinished() {
		finishedFlag = 1
		reason = string(a.message.FinishReason())
	}
	// A summary's collapsed/expanded state changes which of the two
	// render shapes runs, and no section's own key moves when it flips,
	// so it belongs here. toggleSummaryExpanded also drops the caches
	// outright; this keeps the key honest for anything that reaches a
	// render without going through the toggle (a rebuilt item, say).
	var summaryFlag byte
	if a.summaryExpanded {
		summaryFlag = 1
	}
	// Length-prefixed framing keeps the flags and the reason string from
	// blending into one another.
	return fnvFields([]byte{finishedFlag}, []byte{summaryFlag}, []byte(reason))
}

// renderMessageContent renders the message content including thinking, main
// content, and finish reason. Each section is served from its own cache;
// only the section whose source text or extras changed since the last
// render is recomputed.
func (a *AssistantMessageItem) renderMessageContent(width int, keys assistantSectionKeys) (string, int) {
	var messageParts []string
	thinking := strings.TrimSpace(a.message.ReasoningContent().Thinking)
	content := strings.TrimSpace(a.message.Content().Text)

	if thinking != "" {
		messageParts = append(messageParts, a.cachedThinking(width, keys.thinkSrc, keys.thinkExtra))
	}

	if content != "" {
		if thinking != "" {
			messageParts = append(messageParts, "")
		}
		messageParts = append(messageParts, a.cachedContent(width, keys.contentSrc, keys.contentExtra))
	}

	if a.message.IsFinished() {
		switch {
		case a.message.FinishReason() == message.FinishReasonCanceled:
			messageParts = append(messageParts, a.sty.Messages.AssistantCanceled.Render("Canceled"))
		case a.message.IsErrorLike():
			messageParts = append(messageParts, a.cachedError(width, keys.errSrc, keys.errExtra))
		}
	}

	out := strings.Join(messageParts, "\n")
	return out, lipgloss.Height(out)
}

// thinkingKey returns the (srcHash, extra) cache key components for the
// thinking section. extra folds in everything other than the raw
// thinking text that affects the rendered output: the view mode
// (collapsed / tail-window / full) and the footer state (which
// depends on IsThinking, ToolCalls, and ThinkingDuration).
//
// The source hash is computed incrementally: during streaming the
// thinking text only grows by appending, so we continue the FNV-64a
// hash from the saved state rather than re-hashing the entire
// accumulated text. See upstream ticket CHARM-1785.
func (a *AssistantMessageItem) thinkingKey() (uint64, uint64) {
	thinking := a.message.ReasoningContent().Thinking
	srcHash := a.thinkingHashIncremental(thinking)

	showFooter := !a.message.IsThinking() || len(a.message.ToolCalls()) > 0
	var durationStr string
	if showFooter {
		duration := time.Duration(a.message.ThinkingDurationSeconds(time.Now().Unix())) * time.Second
		if duration.String() != "0s" {
			durationStr = duration.String()
		}
	}
	var footer byte
	if showFooter {
		footer = 1
	}
	// Length-prefixed framing avoids any delimiter collision between
	// the flag bytes and the duration string. The view mode is folded
	// in so that toggling collapsed ↔ tail-window ↔ full invalidates
	// only the thinking section, not content/error.
	extra := fnvFields([]byte{byte(a.thinkingViewMode), footer}, []byte(durationStr))
	return srcHash, extra
}

// thinkingHashIncremental returns the FNV-64a hash of thinking,
// continuing from the saved state when thinking is a prefix-extension
// of the previously hashed text. Falls back to a full re-hash when
// the text shrinks or diverges (e.g. user retried the turn).
func (a *AssistantMessageItem) thinkingHashIncremental(thinking string) uint64 {
	// Detect divergence: if the saved sample no longer matches the
	// start of the current text, the content was rewritten (retry)
	// and we must re-hash from scratch. The length check must be
	// strict (>): a same-length rewrite (e.g. a retry that happens to
	// produce equally long thinking text) only differs past the
	// 64-byte sample, and the incremental loop below never runs when
	// thinkingHashLen already equals len(thinking) — so a same-length
	// rewrite must fall through to the full re-hash below, or the
	// stale hash from before the retry would be reused.
	sampleLen := min(len(thinking), 64)
	if a.thinkingHashLen > 0 && len(thinking) > a.thinkingHashLen &&
		thinking[:sampleLen] == a.thinkingHashSample {
		// Fast path: continue hashing from saved state.
		h := a.thinkingHash
		for i := a.thinkingHashLen; i < len(thinking); i++ {
			h ^= uint64(thinking[i])
			h *= 1099511628211
		}
		a.thinkingHash = h
		a.thinkingHashLen = len(thinking)
		return h
	}
	// Full re-hash (first call, or text diverged/shrank).
	a.thinkingHashFullRehashes++
	h := fnv64(thinking)
	a.thinkingHash = h
	a.thinkingHashLen = len(thinking)
	a.thinkingHashSample = thinking[:sampleLen]
	return h
}

// contentKey returns the (srcHash, extra) cache key components for the
// main content section.
func (a *AssistantMessageItem) contentKey() (uint64, uint64) {
	return fnv64(a.message.Content().Text), 0
}

// errorKey returns the (srcHash, extra) cache key components for the
// error / refusal section. Returns (0, 0) when no error-like finish
// is present so the cache stays a no-op for normal messages.
//
//nolint:unparam // extra is always 0 here, but the (srcHash, extra) shape matches contentKey/thinkingKey and assistantSection.hit
func (a *AssistantMessageItem) errorKey() (uint64, uint64) {
	if !a.message.IsFinished() || !a.message.IsErrorLike() {
		return 0, 0
	}
	finishPart := a.message.FinishPart()
	if finishPart == nil {
		return 0, 0
	}
	// Length-prefixed framing prevents Message+Details collisions
	// between distinct (Message, Details) tuples that would
	// otherwise concatenate to the same byte sequence. Fold the
	// reason in so ERROR vs REFUSED banners never share a cache slot.
	return fnvFields([]byte(finishPart.Reason), []byte(finishPart.Message), []byte(finishPart.Details)), 0
}

// cachedThinking returns the rendered thinking section, computing and
// caching it on miss. The thinking-box height (used for click target
// detection) is preserved across hits via assistantSection.aux so the
// cached path never desyncs click detection. srcHash/extra come from
// computeSectionKeys — see its doc comment for why this takes them as
// parameters instead of calling thinkingKey() itself.
func (a *AssistantMessageItem) cachedThinking(width int, srcHash, extra uint64) string {
	if a.thinkingSec.hit(width, srcHash, extra) {
		a.thinkingBoxHeight = a.thinkingSec.aux
		return a.thinkingSec.out
	}
	out := a.renderThinking(a.message.ReasoningContent().Thinking, width)
	a.thinkingSec.store(width, srcHash, extra, out, a.thinkingBoxHeight)
	return out
}

// cachedContent returns the rendered content section. The markdown body is
// painted onto its own background block (see common.BlockBackground) so
// the assistant's prose reads as a distinct panel in the transcript.
// srcHash/extra come from computeSectionKeys (see cachedThinking).
func (a *AssistantMessageItem) cachedContent(width int, srcHash, extra uint64) string {
	if a.contentSec.hit(width, srcHash, extra) {
		return a.contentSec.out
	}
	out := a.renderMarkdown(a.message.Content().Text, width)
	// GetBackground never returns nil: lipgloss.Style.GetBackground reports
	// "no color set" as lipgloss.NoColor{}, a concrete non-nil color.Color
	// (see charm.land/lipgloss/v2's Style.GetBackground doc). A `!= nil`
	// check is therefore always true and would paint every message with
	// NoColor{}'s opaque black RGBA whenever no theme background is
	// configured, so the absence check has to compare against NoColor
	// itself instead.
	if bg := a.sty.Messages.MarkdownBlock.GetBackground(); bg != (lipgloss.NoColor{}) {
		out = common.BlockBackground(out, width, bg)
	}
	a.contentSec.store(width, srcHash, extra, out, 0)
	return out
}

// cachedError returns the rendered error section. srcHash/extra come from
// computeSectionKeys (see cachedThinking).
func (a *AssistantMessageItem) cachedError(width int, srcHash, extra uint64) string {
	if a.errorSec.hit(width, srcHash, extra) {
		return a.errorSec.out
	}
	out := a.renderError(width)
	a.errorSec.store(width, srcHash, extra, out, 0)
	return out
}

// renderThinking renders the thinking/reasoning content with footer.
//
// Slicing happens AFTER glamour rendering so fenced code blocks, list
// continuations, and tables are not split mid-block — the same
// boundary problem §4.4 of the design note flags. The bordered
// ThinkingBox style is applied on top of the (already-windowed)
// lines so the visual box matches what the user sees today.
func (a *AssistantMessageItem) renderThinking(thinking string, width int) string {
	renderer := common.QuietMarkdownRenderer(a.sty, width)
	rendered := a.streamingThinking.Render(thinking, width, renderer)
	rendered = strings.TrimSpace(rendered)

	// Count lines and, for the windowed view modes, slice the tail
	// WITHOUT splitting the entire rendered document. Splitting a
	// 1200-line render just to keep the last 10 lines is O(n) per
	// tick; tailLines finds the cut point with a bounded backward
	// scan. See upstream ticket CHARM-1785.
	var lines []string
	var totalLines int
	switch a.thinkingViewMode {
	case thinkingCollapsed:
		totalLines = countLines(rendered)
		if totalLines > maxCollapsedThinkingHeight {
			tail, hidden := tailLines(rendered, maxCollapsedThinkingHeight, totalLines)
			hint := a.sty.Messages.ThinkingTruncationHint.Render(
				fmt.Sprintf(assistantMessageTruncateFormat, hidden),
			)
			lines = append([]string{hint, ""}, strings.Split(tail, "\n")...)
		} else {
			lines = strings.Split(rendered, "\n")
		}
	case thinkingTailWindow:
		totalLines = countLines(rendered)
		if totalLines > maxExpandedThinkingTailLines {
			tail, hidden := tailLines(rendered, maxExpandedThinkingTailLines, totalLines)
			hint := a.sty.Messages.ThinkingTruncationHint.Render(
				fmt.Sprintf(assistantMessageTailWindowFormat, hidden),
			)
			lines = append([]string{hint, ""}, strings.Split(tail, "\n")...)
		} else {
			lines = strings.Split(rendered, "\n")
		}
	default:
		lines = strings.Split(rendered, "\n")
	}

	thinkingStyle := a.sty.Messages.ThinkingBox
	if a.hovered {
		thinkingStyle = a.sty.Messages.ThinkingBoxHover
	}
	thinkingStyle = thinkingStyle.Width(width)
	result := thinkingStyle.Render(strings.Join(lines, "\n"))
	a.thinkingBoxHeight = lipgloss.Height(result)

	var footer string
	// if thinking is done add the thought for footer
	if !a.message.IsThinking() || len(a.message.ToolCalls()) > 0 {
		duration := time.Duration(a.message.ThinkingDurationSeconds(time.Now().Unix())) * time.Second
		if duration.String() != "0s" {
			footer = a.sty.Messages.ThinkingFooterTitle.Render("Thought for ") +
				a.sty.Messages.ThinkingFooterDuration.Render(duration.String())
		}
	}

	if footer != "" {
		result += "\n\n" + footer
	}

	return result
}

// renderMarkdown renders content as markdown. F8 routes the call
// through streamingContent, which caches the glamour render of a
// "stable prefix" so each streaming flush only re-renders the
// trailing partial. The streaming cache invalidates itself on
// width change and on any content that is not a prefix-extension
// of the previously rendered content (e.g. user retried the
// turn), and falls back to a full render whenever boundary
// detection has the slightest doubt — see
// findSafeMarkdownBoundary.
func (a *AssistantMessageItem) renderMarkdown(content string, width int) string {
	renderer := common.MarkdownRenderer(a.sty, width)
	return a.streamingContent.Render(content, width, renderer)
}

// renderSpinning draws the working animation under the phase the message
// is actually in. The label used to be set only while thinking or
// summarizing, which left the most common case — request sent, nothing
// back yet — as a bare band of glyphs and a timer that said how long
// something unnamed had been taking. [message.Working] is the single
// reading of that state; PhaseWorking's "Working" is the floor, so the
// label is never empty while the animation is up.
//
// SetLabel re-renders the label rune by rune and recomputes the animation
// width, so it is called only when the wording actually changes rather
// than on all twenty frames a second.
func (a *AssistantMessageItem) renderSpinning() string {
	if label := a.message.Working().Label(); label != "" && label != a.animLabel {
		a.animLabel = label
		a.anim.SetLabel(label)
	}
	return a.anim.Render()
}

// renderError renders an error or provider-refusal banner.
func (a *AssistantMessageItem) renderError(width int) string {
	finishPart := a.message.FinishPart()
	tagLabel := "ERROR"
	titleText := finishPart.Message
	detailsText := finishPart.Details
	if finishPart.Reason == message.FinishReasonContentFilter {
		tagLabel = refusalTagLabel
		titleText = cmp.Or(titleText, refusalTitle)
		detailsText = cmp.Or(detailsText, refusalDetails)
	}
	errTag := a.sty.Messages.ErrorTag.Render(tagLabel)
	truncated := ansi.Truncate(titleText, width-2-lipgloss.Width(errTag), "...")
	title := fmt.Sprintf("%s %s", errTag, a.sty.Messages.ErrorTitle.Render(truncated))
	if detailsText == "" {
		return title
	}
	details := a.sty.Messages.ErrorDetails.Width(width - 2).Render(detailsText)
	return fmt.Sprintf("%s\n\n%s", title, details)
}

// isSpinning returns true if the assistant message is still generating.
// spinnerActive reports whether this item is rendering the working
// spinner, and therefore needs animation ticks.
//
// It is isSpinning plus the one case that test cannot see: a collapsed
// summary is *only* the spinner for the whole pass (renderCollapsedSummary),
// while isSpinning goes false as soon as the summary streams its first
// delta, since a message with content is normally past its spinner. Gating
// the ticks on isSpinning alone therefore froze that row on whatever frame
// the first delta happened to catch.
//
// A summary collapsed by hand mid-pass starts ticking again on the next
// delta, through SetMessage's transition check below; during a summarize
// those arrive constantly, and toggling expansion cannot return a command
// of its own (see Expandable).
func (a *AssistantMessageItem) spinnerActive() bool {
	return a.isSpinning() || (a.summaryIsCollapsed() && !a.message.IsFinished())
}

func (a *AssistantMessageItem) isSpinning() bool {
	isThinking := a.message.IsThinking()
	isFinished := a.message.IsFinished()
	hasContent := strings.TrimSpace(a.message.Content().Text) != ""
	hasToolCalls := len(a.message.ToolCalls()) > 0
	return (isThinking || !isFinished) && !hasContent && !hasToolCalls
}

// SetMessage is used to update the underlying message. Only the
// sub-section caches whose source text or extras changed are
// invalidated; the others survive and serve cache hits on the next
// RawRender.
func (a *AssistantMessageItem) SetMessage(msg *message.Message) tea.Cmd {
	wasSpinning := a.spinnerActive()
	a.message = msg
	// Bump the F6 version even if the underlying *message.Message
	// pointer is identical: callers may have mutated the message in
	// place (delta append) and we cannot tell from here. The
	// per-section caches dedupe identical content via FNV-64 hashes,
	// so a redundant bump only costs one list-cache repopulation.
	a.Bump()
	// The prefix cache is keyed by a fingerprint that includes every
	// section's source hash, so an unchanged section keeps its prefix
	// cache valid while a changed section forces a miss naturally.
	// Section caches themselves are content-keyed, so they do not
	// need an explicit drop here either.
	if !wasSpinning && a.spinnerActive() {
		return a.StartAnimation()
	}
	return nil
}

// Finished implements list.Item. The assistant message is freezable
// once the message reports IsFinished() and is no longer spinning
// (no animation tick remains pending). Streaming tail animation is
// caught by isSpinning, so freezing only kicks in once the turn is
// fully terminal. The list cache invalidates the entry on the next
// version bump if anything (focus, highlight, expansion) changes.
func (a *AssistantMessageItem) Finished() bool {
	return a.message.IsFinished() && !a.isSpinning()
}

// AlwaysSpaced implements list.AlwaysSpaced. Assistant text keeps the
// list's normal gap around it even on a short single-line reply, so it
// never blends into a dense run of one-line tool calls (see
// list.List.gapAt).
func (a *AssistantMessageItem) AlwaysSpaced() bool {
	return true
}

// clearCache drops every cached render for this item, including the
// per-section caches. Shadows the embedded cachedMessageItem.clearCache
// so ClearItemCaches (style change) wipes the section caches too.
// F8: also drop the streaming-markdown stable-prefix cache because
// the cached glamour render embeds the OLD style's ANSI sequences
// and is no longer visually consistent with the new style.
func (a *AssistantMessageItem) clearCache() {
	a.cachedMessageItem.clearCache()
	a.thinkingSec.reset()
	a.contentSec.reset()
	a.errorSec.reset()
	a.streamingContent.Reset()
	a.streamingThinking.Reset()
	a.thinkingHash = 0
	a.thinkingHashLen = 0
	a.thinkingHashSample = ""
	a.thinkingHashFullRehashes = 0
}

// ToggleExpanded advances the F5 thinking view-mode cycle and returns
// whether the item is now in any expanded state (tail-window or full).
// The cycle is collapsed → tail-window → full → collapsed, with the
// tail-window step skipped when the rendered thinking fits within
// maxExpandedThinkingTailLines so short blocks remain a two-click
// toggle. Both the thinking section cache and the F3 prefix cache
// fold thinkingViewMode into their keys, so no explicit invalidation
// is required here.
//
// When the message carries no thinking text the toggle is a no-op:
// there is nothing to expand, and mutating the view mode would
// thrash the thinking-section cache key for no visible benefit.
func (a *AssistantMessageItem) ToggleExpanded() bool {
	// A summary's toggle is about the summary, not about its reasoning:
	// routing it through the thinking cycle would make the row
	// unopenable whenever the summarize pass happened not to think.
	if a.isSummary() {
		return a.toggleSummaryExpanded()
	}
	if strings.TrimSpace(a.message.ReasoningContent().Thinking) == "" {
		return a.thinkingViewMode != thinkingCollapsed
	}
	switch a.thinkingViewMode {
	case thinkingCollapsed:
		if a.tailWindowWouldTruncate() {
			a.thinkingViewMode = thinkingTailWindow
		} else {
			a.thinkingViewMode = thinkingFullExpanded
		}
	case thinkingTailWindow:
		a.thinkingViewMode = thinkingFullExpanded
	case thinkingFullExpanded:
		a.thinkingViewMode = thinkingCollapsed
	}
	// View-mode changes alter the windowing slice applied after
	// glamour render. The streaming prefix cache may have been
	// seeded under a different slice regime, and glued renders are
	// not byte-identical to monolithic ones. Drop the prefix cache
	// so the next render is clean.
	a.streamingThinking.Reset()
	a.Bump()
	return a.thinkingViewMode != thinkingCollapsed
}

// tailWindowWouldTruncate reports whether the current thinking text
// is long enough that the tail-window step is worth inserting into
// the toggle cycle. We use a cheap source-text logical-line count
// as the heuristic rather than peeking into the cache: the cache
// may be populated in collapsed state (where its height is bounded
// by maxCollapsedThinkingHeight and tells us nothing about the
// underlying length), and re-running glamour just to count lines
// would defeat the cache. The heuristic can over-trigger (a source
// with many short lines may wrap to fewer than N lines), in which
// case the tail-window render is visually identical to full and
// the cycle costs the user one extra toggle — preferred over the
// alternative of failing to show the affordance on a genuinely
// long block.
//
// Logical line count is `1 + newlineCount` (a string with no
// newlines is one line). Comparing newline count alone introduced
// an off-by-one that let a source whose post-newline-split length
// equalled the cap skip the tail-window step.
func (a *AssistantMessageItem) tailWindowWouldTruncate() bool {
	lineCount := 1 + strings.Count(a.message.ReasoningContent().Thinking, "\n")
	return lineCount > maxExpandedThinkingTailLines
}

// HandleMouseClick implements MouseClickable. It signals (via a true return)
// that the click lies on the thinking box so the caller can invoke
// [AssistantMessageItem.ToggleExpanded] through the generic [Expandable]
// path. Toggling here directly would double-toggle because the caller always
// runs the generic path after a handled click.
func (a *AssistantMessageItem) HandleMouseClick(btn ansi.MouseButton, x, y int) bool {
	if btn != ansi.MouseLeft {
		return false
	}
	// A summary's header is a control, and reaching for a disclosure
	// triangle with the pointer is what people do before they read the
	// hint beside it. Its own rows only: a click in the opened text
	// below must not close the thing the person is reading.
	if a.isSummary() {
		return a.summaryHit(y)
	}
	// Otherwise only the thinking box is clickable; other regions of the
	// assistant message should not trigger expansion.
	return a.thinkingBoxHeight > 0 && y < a.thinkingBoxHeight
}

// HoverableAt matches whatever HandleMouseClick treats as the click
// target, so the row highlights exactly where clicking does something.
func (a *AssistantMessageItem) HoverableAt(_ int, y, _ int) bool {
	if a.isSummary() {
		return a.summaryHit(y)
	}
	return a.thinkingBoxHeight > 0 && y >= 0 && y < a.thinkingBoxHeight
}

// SetHovered updates thinking-box hover feedback.
func (a *AssistantMessageItem) SetHovered(hovered bool) {
	if a.hovered == hovered {
		return
	}
	a.hovered = hovered
	a.thinkingSec.reset()
	a.cachedMessageItem.clearCache()
	a.Bump()
}

// HandleKeyEvent implements KeyEventHandler.
func (a *AssistantMessageItem) HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd) {
	if k := key.String(); k == "c" || k == "y" {
		text := a.message.Content().Text
		return true, common.CopyToClipboard(text, "Message copied to clipboard")
	}
	return false, nil
}
