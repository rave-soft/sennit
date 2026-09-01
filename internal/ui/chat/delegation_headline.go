package chat

import "strings"

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
