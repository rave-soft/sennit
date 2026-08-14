package agent

import (
	"errors"
	"fmt"
)

var (
	ErrSessionBusy    = errors.New("session is currently processing another request")
	ErrEmptyPrompt    = errors.New("prompt is empty")
	ErrSessionMissing = errors.New("session id is missing")
)

// ProviderQuotaError reports that a provider rejected a request because
// the requested model is not enabled under the account's current plan
// (e.g. a model not yet turned on in GitHub Copilot's settings). The
// agent core stays free of display concerns — see the "TUI owns the
// display copy" note in Run — and returns this typed error so callers
// can recognize it via errors.As and render their own styled UI
// (hyperlink, color, etc.) instead of agent baking terminal styling
// into persisted message content.
type ProviderQuotaError struct {
	Provider    string // e.g. "copilot".
	Model       string // Catwalk model name/ID the provider rejected.
	SettingsURL string // Where the user can enable the model.
}

func (e *ProviderQuotaError) Error() string {
	return fmt.Sprintf("%q is not enabled for %s; enable it at %s", e.Model, e.Provider, e.SettingsURL)
}
