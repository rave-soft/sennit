package chat

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
