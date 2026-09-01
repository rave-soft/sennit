package proto

import (
	"time"

	"github.com/rave-soft/sennit/internal/session"
)

type Todo = session.Todo

// LSPState is how far along an LSP server is, as the UI needs to know it.
//
// It is a string rather than the iota the runtime uses, for the same
// reason ThreadStatus is: a number is only meaningful next to the const
// block that defined it, and this one crosses into a frontend that must
// not import the LSP runtime to read it.
// TestLSPStateParityWithDomain keeps the two in step.
type LSPState string

const (
	LSPStateUnstarted LSPState = "unstarted"
	LSPStateStarting  LSPState = "starting"
	LSPStateReady     LSPState = "ready"
	LSPStateError     LSPState = "error"
	LSPStateStopped   LSPState = "stopped"
	LSPStateDisabled  LSPState = "disabled"
)

// LSPDiagnosticCounts is how many diagnostics a server currently reports,
// by severity.
type LSPDiagnosticCounts struct {
	Error       int `json:"error,omitempty"`
	Warning     int `json:"warning,omitempty"`
	Information int `json:"information,omitempty"`
	Hint        int `json:"hint,omitempty"`
}

// LSPClientInfo is one server's state as the frontend sees it.
//
// It deliberately carries no client handle. The runtime type it is built
// from has one, and while the workspace contract aliased that type
// straight through, every consumer of the contract — the sidebar included
// — was handed a live LSP client it had no business holding. Error is a
// string here for the same reason: what reaches a frontend is text.
type LSPClientInfo struct {
	Name            string    `json:"name"`
	State           LSPState  `json:"state"`
	Error           string    `json:"error,omitempty"`
	DiagnosticCount int       `json:"diagnostic_count,omitempty"`
	ConnectedAt     time.Time `json:"connected_at"`
}
