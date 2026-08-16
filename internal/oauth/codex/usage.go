package codex

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Usage is what the Codex backend reports about the account's plan and how
// much of its allowance is gone. It rides along on every response, so it
// costs nothing to collect and is always as fresh as the last request.
type Usage struct {
	// Plan is the subscription tier, as the backend spells it ("plus",
	// "pro", "business", …).
	Plan string
	// Primary and Secondary are the rate-limit windows the plan is
	// measured in — typically a long one (weekly) and a short one
	// (hourly). A window the account does not have reports Known() false.
	Primary   UsageWindow
	Secondary UsageWindow
	// CapturedAt is when this snapshot arrived, so a caller can tell a
	// current reading from one left over from a much earlier request.
	CapturedAt time.Time
}

// UsageWindow is one rate-limit window.
type UsageWindow struct {
	UsedPercent   int
	WindowMinutes int
	// ResetsAt is when the window rolls over. Zero when the backend did
	// not say.
	ResetsAt time.Time
}

// Known reports whether this window exists for the account. A plan with
// only a weekly limit reports a zero-length secondary window, which is not
// the same as one that is merely unused.
func (w UsageWindow) Known() bool { return w.WindowMinutes > 0 }

// Known reports whether anything was captured at all.
func (u Usage) Known() bool { return u.Plan != "" || u.Primary.Known() }

const (
	planHeader = "X-Codex-Plan-Type"

	primaryUsedHeader    = "X-Codex-Primary-Used-Percent"
	primaryWindowHeader  = "X-Codex-Primary-Window-Minutes"
	primaryResetAtHeader = "X-Codex-Primary-Reset-At"

	secondaryUsedHeader    = "X-Codex-Secondary-Used-Percent"
	secondaryWindowHeader  = "X-Codex-Secondary-Window-Minutes"
	secondaryResetAtHeader = "X-Codex-Secondary-Reset-At"
)

// ParseUsage reads the plan and rate-limit headers the Codex backend sets
// on every response. It reports false when the headers are absent, which is
// the case for any other provider's responses and for errors served before
// the account is resolved.
func ParseUsage(h http.Header) (Usage, bool) {
	usage := Usage{
		Plan: strings.TrimSpace(h.Get(planHeader)),
		Primary: UsageWindow{
			UsedPercent:   headerInt(h, primaryUsedHeader),
			WindowMinutes: headerInt(h, primaryWindowHeader),
			ResetsAt:      headerTime(h, primaryResetAtHeader),
		},
		Secondary: UsageWindow{
			UsedPercent:   headerInt(h, secondaryUsedHeader),
			WindowMinutes: headerInt(h, secondaryWindowHeader),
			ResetsAt:      headerTime(h, secondaryResetAtHeader),
		},
		CapturedAt: time.Now(),
	}
	if !usage.Known() {
		return Usage{}, false
	}
	return usage, true
}

func headerInt(h http.Header, name string) int {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get(name)))
	if err != nil {
		return 0
	}
	return n
}

func headerTime(h http.Header, name string) time.Time {
	seconds := headerInt(h, name)
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0)
}

// latestUsage holds the most recent snapshot for the process.
//
// It is package state because that is what it describes: one machine, one
// signed-in account, one allowance, read by the UI and written by whichever
// request happened last. Threading it through the provider stack to the
// sidebar would touch a dozen types to say the same thing.
var latestUsage struct {
	sync.RWMutex
	usage Usage
	known bool
}

// RecordUsage stores a snapshot as the current one.
func RecordUsage(u Usage) {
	latestUsage.Lock()
	defer latestUsage.Unlock()
	latestUsage.usage = u
	latestUsage.known = true
}

// LatestUsage returns the most recent snapshot, and whether there is one.
// Nothing is reported until the account has made a request, since the
// allowance is only ever quoted in a response.
func LatestUsage() (Usage, bool) {
	latestUsage.RLock()
	defer latestUsage.RUnlock()
	return latestUsage.usage, latestUsage.known
}

// NewUsageTransport wraps base so every Codex response updates the current
// snapshot. base may be nil, which means http.DefaultTransport.
func NewUsageTransport(base http.RoundTripper) http.RoundTripper {
	return &usageTransport{base: base}
}

type usageTransport struct {
	base http.RoundTripper
}

func (t *usageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if usage, ok := ParseUsage(resp.Header); ok {
		RecordUsage(usage)
	}
	return resp, err
}
