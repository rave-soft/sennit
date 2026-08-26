package codex

import (
	"context"
	"fmt"
	"io"
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

// usageStore holds the most recent snapshot per account.
//
// It is package state because that is what it describes: one machine,
// possibly several signed-in accounts, each with its own allowance, read by
// the UI and written by whichever request happened last for that account.
// Threading it through the provider stack to the sidebar would touch a
// dozen types to say the same thing.
//
// The empty accountID is a legal key, not a missing one: personal plans
// never send the chatgpt-account-id header (see Headers), so their snapshot
// has to live somewhere too, and "" is where it lands.
var usageStore struct {
	sync.RWMutex
	byAccount map[string]Usage
	latest    string
	hasLatest bool
}

// RecordUsageFor stores a snapshot for one account.
func RecordUsageFor(accountID string, u Usage) {
	usageStore.Lock()
	defer usageStore.Unlock()
	if usageStore.byAccount == nil {
		usageStore.byAccount = make(map[string]Usage)
	}
	usageStore.byAccount[accountID] = u
	usageStore.latest = accountID
	usageStore.hasLatest = true
}

// RecordUsage stores a snapshot under the empty accountID, the personal-plan
// case. Production code records through the transport, which knows whose
// response it is holding; this wrapper's only remaining callers are tests in
// other packages that stage a snapshot for the sidebar to render.
func RecordUsage(u Usage) {
	RecordUsageFor("", u)
}

// UsageFor returns the snapshot for one account, and whether there is one.
func UsageFor(accountID string) (Usage, bool) {
	usageStore.RLock()
	defer usageStore.RUnlock()
	u, ok := usageStore.byAccount[accountID]
	return u, ok
}

// LatestUsage returns the snapshot for whichever account made the most
// recent request, and whether there is one. Nothing is reported until some
// account has made a request, since the allowance is only ever quoted in a
// response.
func LatestUsage() (Usage, bool) {
	usageStore.RLock()
	defer usageStore.RUnlock()
	if !usageStore.hasLatest {
		return Usage{}, false
	}
	return usageStore.byAccount[usageStore.latest], true
}

// resetUsage clears the store. Tests that touch package state call this so
// they do not leak snapshots into one another; such tests must not run
// t.Parallel().
func resetUsage() {
	usageStore.Lock()
	defer usageStore.Unlock()
	usageStore.byAccount = nil
	usageStore.latest = ""
	usageStore.hasLatest = false
}

// FetchUsage asks the Codex backend for one account's current allowance, by
// sending the same request FetchModels does and reading only its X-Codex-*
// response headers.
//
// It deliberately does NOT go through NewUsageTransport and does NOT call
// RecordUsageFor: the account being asked about here need not be the active
// one — this is how account management checks a not-currently-used
// account's limits, and how rotation will later pick among several — so
// writing the result into the store would silently make it "latest" and
// hand the sidebar someone else's numbers. Do not "optimize" this to reuse
// NewUsageTransport; that would reintroduce exactly that bug.
//
// The Codex backend is confirmed to set these headers on /responses; /models
// carrying them too is an assumption, not a documented guarantee (see
// TestLiveFetchUsage). Their absence is therefore reported as
// (Usage{}, false, nil), not an error — an honest "unknown" rather than a
// guess.
func FetchUsage(ctx context.Context, proxyURL, accessToken, accountID string) (Usage, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	req, client, err := newModelsRequest(ctx, proxyURL, accessToken, accountID)
	if err != nil {
		return Usage{}, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Usage{}, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return Usage{}, false, fmt.Errorf("codex usage fetch failed: %s: %s", resp.Status, truncate(string(body), 200))
	}
	usage, ok := ParseUsage(resp.Header)
	return usage, ok, nil
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
	// Read the account off the request before handing it to base: after
	// RoundTrip returns, req is no longer ours to touch.
	accountID := req.Header.Get(AccountIDHeader)
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if usage, ok := ParseUsage(resp.Header); ok {
		RecordUsageFor(accountID, usage)
	}
	return resp, err
}
