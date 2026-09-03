package accounts

import (
	"fmt"
	"sync"
	"time"
)

// DefaultRotationDebounce is how long Rotator.Pick refuses to rotate again
// after a successful pick, when the caller hasn't configured one of their
// own. It exists so a burst of responses arriving close together (retries,
// a batch of tool calls) cannot spin through every account in a fraction
// of a second; see Rotator's doc comment.
const DefaultRotationDebounce = 5 * time.Second

// RotationPolicy is the caller-translated form of a provider's rotation
// settings. This package must not import internal/config — see the
// package doc on Account — so config.RotationConfig is never referenced
// here; the caller converts its own config struct into this one at the
// boundary. The name deliberately differs from config.RotationConfig's:
// the conversion site imports both packages, and two types with one name
// on either side of an assignment is where a mistranslated field hides.
type RotationPolicy struct {
	// MinRemainingPercent is the remaining-allowance threshold, meaningful
	// only for RotateThreshold providers: an account counts as exhausted
	// once a known usage window's UsedPercent is at or above
	// 100-MinRemainingPercent. Zero (the unset value) means
	// DefaultMinRemainingPercent.
	MinRemainingPercent int
	// Cooldown is how long MarkRateLimited holds an account down when the
	// caller has no Retry-After to go on. Zero means DefaultCooldown.
	Cooldown time.Duration
	// Order lists account IDs in the order Pick should prefer them.
	// Accounts the store holds but Order does not name are tried after
	// every named account, in the store's own order — Order reorders,
	// it never hides an account that exists.
	Order []string
}

// threshold returns the used-percent value at or above which a known
// window counts as exhausted.
func (c RotationPolicy) threshold() int {
	p := c.MinRemainingPercent
	if p <= 0 {
		p = DefaultMinRemainingPercent
	}
	return 100 - p
}

// cooldown returns the duration MarkRateLimited falls back to when the
// caller supplies no Retry-After.
func (c RotationPolicy) cooldown() time.Duration {
	if c.Cooldown <= 0 {
		return DefaultCooldown
	}
	return c.Cooldown
}

// ErrAllExhausted is returned by Pick when every candidate account is
// disabled, cooling down, or below the configured threshold. It carries
// the earliest time any candidate becomes available again, so a caller
// can tell the user when to retry instead of guessing.
//
// Decision Р4 in the plan: when everything is exhausted the request must
// not be sent on the least-exhausted account anyway — the caller is
// expected to surface this error, not fall through to a pick.
type ErrAllExhausted struct {
	// ProviderID identifies which provider ran out of accounts.
	ProviderID string
	// ResetsAt is the earliest time any exhausted candidate becomes
	// usable again: the soonest usage-window reset for a threshold
	// provider, or the soonest cooldown expiry for a rate-limit provider.
	// It is the zero time when no candidate carries a known reset time.
	ResetsAt time.Time
}

// Error implements the error interface.
func (e *ErrAllExhausted) Error() string {
	if e.ResetsAt.IsZero() {
		return fmt.Sprintf("all accounts for %q are exhausted", e.ProviderID)
	}
	return fmt.Sprintf("all accounts for %q are exhausted until %s",
		e.ProviderID, e.ResetsAt.Format("15:04"))
}

// Rotator decides which account a provider should use next. It holds no
// reference to a Store and does no I/O: callers pass it the current
// account list and apply whatever it decides through their own mechanism
// (see plan §5.2) — that keeps this package pure logic, easy to test
// without a filesystem, and safely reusable from multiple call sites that
// share one in-process view of cooldown state.
//
// Cooldown state (which account is rate-limited until when) lives only in
// this struct's memory, never on disk in accounts.json: it records this
// process's own observations (a 429 seen just now), and a cooldown read
// back from disk after a restart would be stale — a fresh process starts
// with a clean slate and relearns cooldowns from whatever it observes
// next. Two Rotators (e.g. two processes) never share this state, which
// is fine: each converges on the same picture once it sees a response.
//
// A Rotator is safe for concurrent use.
type Rotator struct {
	cfg      RotationPolicy
	debounce time.Duration
	now      func() time.Time

	mu           sync.Mutex
	cooldowns    map[string]time.Time // accountID -> cools down until
	lastRotation time.Time
}

// RotatorOption configures a Rotator at construction time.
type RotatorOption func(*Rotator)

// WithClock overrides the time source Rotator uses for cooldown expiry,
// debounce, and threshold windows. Production code leaves this unset and
// gets time.Now; tests inject a fake clock so debounce and cooldown
// assertions never depend on wall-clock sleeps.
func WithClock(now func() time.Time) RotatorOption {
	return func(r *Rotator) { r.now = now }
}

// WithDebounce overrides DefaultRotationDebounce.
func WithDebounce(d time.Duration) RotatorOption {
	return func(r *Rotator) { r.debounce = d }
}

// NewRotator constructs a Rotator for one provider's rotation config.
func NewRotator(cfg RotationPolicy, opts ...RotatorOption) *Rotator {
	r := &Rotator{
		cfg:       cfg,
		debounce:  DefaultRotationDebounce,
		now:       time.Now,
		cooldowns: make(map[string]time.Time),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ShouldRotate reports whether active has fallen below the configured
// threshold on at least one KNOWN usage window (plan Р6: either window,
// not both). It is meaningful only for RotateThreshold providers — a
// caller for a RotateRateLimit provider has nothing to compare and should
// not call this.
//
// all is the provider's full account list, exactly as Pick expects it.
// It exists so a provider with a single account (or none) is a total
// no-op even if that lone account happens to be past the threshold —
// there is nowhere to rotate to, so reporting true would just invite a
// caller to call Pick and get the same account back for its trouble.
//
// An unknown window (Known() false) is never treated as 0% used: it is
// simply skipped, not counted as "fine". A window we have never observed
// tells us nothing about its usage, and treating "no data" as "no usage"
// would let a genuinely exhausted window escape detection until its
// sibling window also happened to report.
func (r *Rotator) ShouldRotate(active Account, all []Account) bool {
	if len(all) <= 1 {
		return false
	}
	limit := r.cfg.threshold()
	for _, w := range []UsageWindow{active.Usage.Primary, active.Usage.Secondary} {
		if w.Known() && w.UsedPercent >= limit {
			return true
		}
	}
	return false
}

// MarkRateLimited marks accountID as cooling down after a 429. It is
// meaningful only for RotateRateLimit providers.
//
// retryAfter is the duration parsed from the response's Retry-After
// header; pass 0 (or a negative value) when the response carried none, and
// MarkRateLimited falls back to RotationPolicy.Cooldown (DefaultCooldown
// when that's unset too) so callers don't each need their own copy of
// that fallback logic.
func (r *Rotator) MarkRateLimited(accountID string, retryAfter time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := retryAfter
	if d <= 0 {
		d = r.cfg.cooldown()
	}
	r.cooldowns[accountID] = r.now().Add(d)
}

// coolingDown reports whether accountID is currently cooling down, per
// r.now(). Must be called with r.mu held.
func (r *Rotator) coolingDown(accountID string) bool {
	until, ok := r.cooldowns[accountID]
	return ok && r.now().Before(until)
}

// Pick chooses the next account to use out of candidates, which must be
// the provider's full, current account list (in whatever order the store
// holds them). Pick does not consult a Store itself so callers control
// exactly what "current" means, including in tests.
//
// Order of consideration: accounts named in RotationPolicy.Order first,
// in that order, then any remaining candidate not named there, in the
// order it appears in candidates. An account present in candidates but
// absent from Order is never dropped — Order only reorders, matching the
// plan's requirement that a misconfigured or stale Order can't strand an
// account.
//
// A candidate is skipped when it is Disabled, currently cooling down
// (MarkRateLimited), or — for a RotateThreshold provider, signaled by a
// non-nil threshold check via ShouldRotate's own logic — below threshold
// on a known window. An account with no usage snapshot at all
// (Usage.Known() false) counts as usable: we haven't learned it's
// exhausted, so treating it as available lets a freshly added account
// actually get used instead of being stranded until something else
// records a snapshot for it.
//
// A single usable candidate, or a provider with only one account
// configured at all, is a total no-op: Pick returns it directly without
// consulting the debounce timer, matching the plan's requirement that a
// one-account provider never rotates. A sole account that is Disabled is
// not usable, though: Pick returns *ErrAllExhausted for it exactly as it
// would for any other exhausted candidate.
//
// When nothing is usable, Pick returns *ErrAllExhausted carrying the
// earliest time any candidate becomes available again (the soonest usage
// window reset, or the soonest cooldown expiry), zero when unknown.
//
// Pick debounces: once it has rotated (returned an account other than the
// currently active one — see currentID), it refuses to rotate again for
// r.debounce. A debounced call returns currentID's account unchanged if
// currentID is itself still usable, or the ErrAllExhausted verdict
// otherwise — debounce only suppresses picking a *different* account, it
// never forces a caller onto an exhausted one.
func (r *Rotator) Pick(providerID, currentID string, candidates []Account) (Account, error) {
	if len(candidates) == 0 {
		return Account{}, &ErrAllExhausted{ProviderID: providerID}
	}
	if len(candidates) == 1 {
		// The one-account no-op still has to honor Disabled: a disabled
		// sole account is exactly the "everything is exhausted" case
		// ErrAllExhausted exists for, not a free pass to send requests
		// on credentials the user turned off.
		if candidates[0].Disabled {
			return Account{}, &ErrAllExhausted{ProviderID: providerID}
		}
		return candidates[0], nil
	}

	ordered := orderCandidates(r.cfg.Order, candidates)
	limit := r.cfg.threshold()

	r.mu.Lock()
	defer r.mu.Unlock()

	debounced := !r.lastRotation.IsZero() && r.now().Sub(r.lastRotation) < r.debounce

	var usable []Account
	var earliestReset time.Time
	for _, a := range ordered {
		if r.usableLocked(a, limit) {
			usable = append(usable, a)
			continue
		}
		// A disabled account will never be picked no matter when its usage
		// window resets - counting it here used to surface a ResetsAt the
		// user could never benefit from ("resets at 15:04" for an account
		// that stays exhausted, by the user's own choice, forever).
		if a.Disabled {
			continue
		}
		if reset := earliestResetFor(a, r.cooldowns); !reset.IsZero() {
			if earliestReset.IsZero() || reset.Before(earliestReset) {
				earliestReset = reset
			}
		}
	}

	if len(usable) == 0 {
		return Account{}, &ErrAllExhausted{ProviderID: providerID, ResetsAt: earliestReset}
	}

	if debounced {
		// Stay on the current account if it's still usable; otherwise
		// the debounce window can't help — there's nothing to hold onto.
		for _, a := range usable {
			if a.ID == currentID {
				return a, nil
			}
		}
	}

	picked := usable[0]
	if picked.ID != currentID {
		r.lastRotation = r.now()
	}
	return picked, nil
}

// usableLocked reports whether a can be picked: not disabled, not
// cooling down, and not below limit on any known usage window. Must be
// called with r.mu held (coolingDown reads r.cooldowns).
func (r *Rotator) usableLocked(a Account, limit int) bool {
	if a.Disabled {
		return false
	}
	if r.coolingDown(a.ID) {
		return false
	}
	for _, w := range []UsageWindow{a.Usage.Primary, a.Usage.Secondary} {
		if w.Known() && w.UsedPercent >= limit {
			return false
		}
	}
	return true
}

// earliestResetFor returns the soonest time a's exhaustion (threshold or
// cooldown) is known to clear, or the zero time if that's unknown.
func earliestResetFor(a Account, cooldowns map[string]time.Time) time.Time {
	var earliest time.Time
	consider := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	if until, ok := cooldowns[a.ID]; ok {
		consider(until)
	}
	consider(a.Usage.Primary.ResetsAt)
	consider(a.Usage.Secondary.ResetsAt)
	return earliest
}

// orderCandidates returns candidates arranged with every account named in
// order placed first (in order's sequence), followed by any remaining
// candidate not named there, in candidates' own order. It never drops a
// candidate that order fails to mention.
func orderCandidates(order []string, candidates []Account) []Account {
	byID := make(map[string]Account, len(candidates))
	for _, a := range candidates {
		byID[a.ID] = a
	}

	out := make([]Account, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, id := range order {
		if a, ok := byID[id]; ok && !seen[id] {
			out = append(out, a)
			seen[id] = true
		}
	}
	for _, a := range candidates {
		if !seen[a.ID] {
			out = append(out, a)
			seen[a.ID] = true
		}
	}
	return out
}

// Compile-time check that ErrAllExhausted satisfies error, so callers can
// rely on errors.As(err, &target) picking it out of a wrapped chain.
var _ error = (*ErrAllExhausted)(nil)
