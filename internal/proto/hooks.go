package proto

// HookMetadata is the wire shape of what hooks did to a tool call. It is
// embedded in a tool response's metadata JSON by internal/agent and read
// back by the chat renderer, which is the whole reason it exists as a
// separate type from the hooks package's own AggregateResult: the two
// sides of that JSON are in different layers, and the UI must not import
// the hook runner to name the shape it decodes.
//
// The field names and json tags are the format. Changing either is a
// wire change: metadata written by an older binary is still read by this
// one, since a session's tool results are stored, not recomputed.
type HookMetadata struct {
	HookCount    int        `json:"hook_count"`
	Decision     string     `json:"decision"`
	Halt         bool       `json:"halt,omitempty"`
	Reason       string     `json:"reason,omitempty"`
	InputRewrite bool       `json:"input_rewrite,omitempty"`
	Hooks        []HookInfo `json:"hooks,omitempty"`
}

// HookInfo identifies a single hook that ran and its individual result.
type HookInfo struct {
	Name         string `json:"name"`
	Matcher      string `json:"matcher,omitempty"`
	Decision     string `json:"decision"`
	Halt         bool   `json:"halt,omitempty"`
	Reason       string `json:"reason,omitempty"`
	InputRewrite bool   `json:"input_rewrite,omitempty"`
}
