package chat

import (
	"encoding/json"

	"github.com/rave-soft/sennit/internal/ui/styles"
)

// simpleToolRenderer renders the shape shared by tools with no
// special-cased body: pending spinner, a header built from a handful of
// params, the shared early-state block (error/canceled/waiting), and
// finally a " · summary" suffix once a result lands. It replaces a
// once-per-tool copy of that skeleton (see the LSP family in lsp.go) with
// one implementation plus a row per tool.
//
// Unmarshal-error policy: params decode errors are ignored, matching what
// every tool folded into this table already did. A malformed call still
// renders — with whatever params fields the input string did contain —
// rather than an inline "Invalid parameters" error. That is deliberate for
// this table: these tools' params only feed a cosmetic header, unlike
// glob/grep/fetch/generic (chat/search.go, chat/fetch.go, chat/generic.go),
// which report bad input because a wrong header there could misrepresent
// what a destructive-looking call is about to touch. Folding this table's
// tools into that stricter policy instead would change nothing today: none
// of them exercises the error path in existing tests, and their renderers
// already ignored the error, so no tool's rendering changes by adopting
// this rule.
type simpleToolRenderer struct {
	title string
	// params decodes opts.ToolCall.Input into the header's param list.
	params func(input string) []string
	// summary computes the " · outcome" suffix from a landed result.
	// Only called once opts.HasEmptyResult() is false.
	summary func(opts *ToolRenderOpts) string
}

// RenderTool implements the [ToolRenderer] interface.
func (r *simpleToolRenderer) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, r.title, opts)
	}

	header := toolHeader(sty, opts.Status, r.title, cappedWidth, opts, r.params(opts.ToolCall.Input)...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if opts.HasEmptyResult() {
		return header
	}

	return appendResultSummary(sty, header, r.summary(opts))
}

// decodeParams unmarshals a tool call's raw JSON input into T, ignoring
// decode errors — see the unmarshal-error policy on [simpleToolRenderer].
func decodeParams[T any](input string) T {
	var params T
	_ = json.Unmarshal([]byte(input), &params)
	return params
}

// contentLineCountSummary is the [simpleToolRenderer.summary] most rows
// use: a plain line count of the result content.
func contentLineCountSummary(opts *ToolRenderOpts) string {
	return lineCountSummary(opts.Result.Content)
}
