package agent

import (
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/oauth/codex"
)

// spentWindowPercent is the used-percent value at or above which a window
// counts as spent for the purposes of ProviderLimitError. It is below 100
// deliberately: the backend quotes a rounded figure, and a request refused
// with a 429 while the window reads 99% is refused for that window, not
// for a burst the next second would let through.
const spentWindowPercent = 95

// resumeGrace is added to a window's reset time before a limited turn is
// retried or a wait is quoted. The reset is quoted to the second by the
// backend and the two clocks need not agree; asking again a moment early
// buys nothing but another 429.
const resumeGrace = 30 * time.Second

// ProviderLimitError is a 429 the provider's own usage headers explain:
// the account's subscription window is spent, and the headers say when it
// rolls over. It exists because "Rate limited" is the wrong thing to tell
// someone whose plan window has hours left on it — the useful facts (which
// window, how full, when it comes back) are already in hand by then.
//
// It wraps the original *fantasy.ProviderError, so every existing
// errors.As on that keeps working, and fantasy.ErrStopRetrying, which is
// what stops the retry pass that produced it (see the Unwrap below).
type ProviderLimitError struct {
	// Provider is the provider id the limit belongs to, and ProviderName
	// its display name, as config spells it.
	Provider     string
	ProviderName string
	// Plan is the subscription tier the backend reports ("plus", "pro",
	// …), empty when it reports none.
	Plan string
	// WindowMinutes is the length of the spent window and UsedPercent how
	// full the backend last reported it. WindowMinutes is zero when the
	// limit is known only through another account's reset time (the
	// all-accounts-exhausted case).
	WindowMinutes int
	UsedPercent   int
	// ResetsAt is when the limit lifts. Never zero: a ProviderLimitError
	// is not built without one, because the reset time is the whole point
	// of telling the user this instead of "rate limited".
	ResetsAt time.Time
	// AllAccounts reports that every configured account for the provider
	// is spent, not just the one the turn ran on.
	AllAccounts bool

	err error
}

// providerLabel is how the provider is named in a message: its configured
// display name, falling back to the id when it has none.
func (e *ProviderLimitError) providerLabel() string {
	if e.ProviderName != "" {
		return e.ProviderName
	}
	return e.Provider
}

// Error renders the limit in the words the TUI shows: which window is
// spent, and when it comes back, in local time and as a countdown.
func (e *ProviderLimitError) Error() string {
	var b strings.Builder
	b.WriteString(e.providerLabel())
	if e.Plan != "" {
		b.WriteString(" ")
		b.WriteString(e.Plan)
	}
	switch {
	case e.AllAccounts:
		b.WriteString(": every account is out of allowance")
	case e.WindowMinutes > 0:
		fmt.Fprintf(&b, ": the %s limit is spent (%d%% used)", limitWindowName(e.WindowMinutes), e.UsedPercent)
	default:
		b.WriteString(": out of allowance")
	}
	fmt.Fprintf(&b, ", resets at %s", e.ResetsAt.Format("15:04"))
	if in := formatWaitUntil(e.ResetsAt, time.Now()); in != "" {
		fmt.Fprintf(&b, " (in %s)", in)
	}
	return b.String()
}

// Unwrap exposes both the original 429 (so the existing provider-error
// handling still sees it) and fantasy.ErrStopRetrying (so the retry pass
// this error is returned from stops instead of backing off).
func (e *ProviderLimitError) Unwrap() []error {
	return []error{e.err, fantasy.ErrStopRetrying}
}

// limitWindowName names a window by its length, the way the plan is sold:
// "5h", "weekly", "hourly". It mirrors the sidebar's planWindowName
// (internal/ui/common) rather than importing it — internal/agent must not
// depend on the TUI.
func limitWindowName(minutes int) string {
	switch {
	case minutes == 60*24*7:
		return "weekly"
	case minutes == 60*24:
		return "daily"
	case minutes == 60:
		return "hourly"
	case minutes%(60*24) == 0:
		return fmt.Sprintf("%dd", minutes/(60*24))
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// formatWaitUntil renders how long is left until at, coarsely and without
// inventing precision: minutes under an hour, hours under a day, days
// beyond that. A reset already past renders empty.
func formatWaitUntil(at, now time.Time) string {
	d := at.Sub(now)
	switch {
	case d <= 0:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(1, int(d.Minutes())))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// spentWindow picks the window a 429 should be attributed to: among the
// windows the account actually has (a Plus plan reports a short one and a
// weekly one, a Pro plan only the weekly one — see codex.UsageWindow.Known),
// the fullest one whose reset still lies ahead.
//
// It reports nothing when no such window exists, which is the honest
// answer for a snapshot that predates the refusal or an account whose plan
// reports no windows at all; the caller then falls back to ordinary
// rate-limit handling rather than quoting a reset it does not have.
func spentWindow(u codex.Usage, now time.Time) (codex.UsageWindow, bool) {
	var best codex.UsageWindow
	var found bool
	for _, w := range []codex.UsageWindow{u.Primary, u.Secondary} {
		if !w.Known() || w.UsedPercent < spentWindowPercent {
			continue
		}
		if w.ResetsAt.IsZero() || !w.ResetsAt.After(now) {
			continue
		}
		if !found || w.UsedPercent > best.UsedPercent {
			best, found = w, true
		}
	}
	return best, found
}
