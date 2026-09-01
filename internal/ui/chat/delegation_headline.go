package chat

import "strings"

// The disclosure triangles and hint wording every collapsible block in
// the chat shares: the compaction row (assistant_summary.go), a
// delegation report (user.go), and the session panel's own collapsible
// sections, which is where the triangles come from (panelSectionHeaderText).
// One affordance, spelled one way, so a reader who has opened one row
// knows how to open the next.
const (
	collapsedGlyph = "▸"
	expandedGlyph  = "▾"
	// The hints sit on lines of their own rather than trailing the
	// header: trailing, they read as a footnote to whatever is beside
	// them and get truncated first on a narrow window. They name
	// clicking alone - the `space` binding works only while the chat
	// list itself holds focus, which is not where someone reading a
	// reply is.
	expandHint   = "Click to expand"
	collapseHint = "Click to collapse"
)

// DelegationHeadline is [delegationHeadline] for the views outside this
// package that show a delegation as a name plus one line of what it was
// asked to do: the session panel's agents and threads sections, and the
// threads dock.
//
// It exists because those views used to take the goal's first line
// verbatim, which for a structured prompt is its first scaffolding line -
// so a row read "middle-developer  ROLE: middle-developer", naming the
// agent twice and the job not at all, while the chat's own block for the
// same delegation had been reading "middle-developer  Устранить
// нарушение границы" all along. One implementation, so the two cannot
// drift again.
func DelegationHeadline(name, prompt string) string {
	return delegationHeadline(name, prompt)
}

// delegationLabel is the one line a delegation is described by: the
// caller's own `description` when the call carries one, and otherwise the
// line worth showing from the prompt.
//
// The description wins because it is written for exactly this: three to
// five words naming the piece of work, by the agent that knows what it
// asked for. Reading it out of a structured prompt is inference by
// comparison - it settles for whatever line the scaffolding left, and it
// has no way to shorten a TASK line that runs to a paragraph.
func delegationLabel(name, description, prompt string) string {
	if trimmed := strings.TrimSpace(description); trimmed != "" {
		return trimmed
	}
	return delegationHeadline(name, prompt)
}

// DelegationLabeler is implemented by the delegation items that resolve
// their own label as the call streams in, so a view outside this package
// (the session panel's agents section) can show the same line the chat
// block shows without parsing the tool call itself on every frame.
type DelegationLabeler interface {
	DelegationLabel() string
}

// DelegationLabel implements [DelegationLabeler].
func (t *AgentToolMessageItem) DelegationLabel() string { return t.headline }
