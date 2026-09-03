package accounts

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func acct(id string) Account {
	return Account{ID: id, APIKey: "$KEY"}
}

func withUsage(a Account, primaryUsed int) Account {
	a.Usage = Usage{
		Plan:    "plus",
		Primary: UsageWindow{UsedPercent: primaryUsed, WindowMinutes: 60},
	}
	return a
}

// TestRotator_ShouldRotate_UnknownUsageIsUsable pins the optimistic
// default: an account with no usage snapshot at all must not look
// exhausted, or a freshly added account would be stranded forever.
func TestRotator_ShouldRotate_UnknownUsageIsUsable(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	fresh := acct("a1") // Usage is the zero value: unknown.
	other := acct("a2")
	require.False(t, r.ShouldRotate(fresh, []Account{fresh, other}))
}

// TestRotator_ShouldRotate_AboveThresholdOnPrimary confirms the basic
// threshold comparison fires once a known window is at or above the
// used-percent cutoff (default: remaining below 10% => used >= 90).
func TestRotator_ShouldRotate_AboveThresholdOnPrimary(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	active := withUsage(acct("a1"), 95)
	other := acct("a2")
	require.True(t, r.ShouldRotate(active, []Account{active, other}))
}

// TestRotator_ShouldRotate_UnknownWindowNotTreatedAsZero exercises Р6:
// checking "both windows" must not silently read an unreported window as
// 0% used. Primary is exhausted; Secondary was never populated
// (WindowMinutes 0, Known() false). If the implementation looped over
// both windows and let a zero-value Secondary mask Primary's real value,
// or effectively ANDed the two windows together, this must still return
// true from Primary alone.
func TestRotator_ShouldRotate_UnknownWindowNotTreatedAsZero(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	active := withUsage(acct("a1"), 95) // Secondary left zero-value/unknown.
	other := acct("a2")
	require.True(t, r.ShouldRotate(active, []Account{active, other}))
}

// TestRotator_ShouldRotate_BelowThreshold is the negative case: usage is
// known but comfortably under the cutoff.
func TestRotator_ShouldRotate_BelowThreshold(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	active := withUsage(acct("a1"), 42)
	other := acct("a2")
	require.False(t, r.ShouldRotate(active, []Account{active, other}))
}

// TestRotator_ShouldRotate_SingleAccountNoOp: with nowhere to rotate to,
// ShouldRotate must report false even for an account past the threshold.
func TestRotator_ShouldRotate_SingleAccountNoOp(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	active := withUsage(acct("only"), 99)
	require.False(t, r.ShouldRotate(active, []Account{active}))
}

// TestRotator_Pick_SingleAccountNoOp: one account over threshold is
// returned as-is, never an error — there's nowhere to rotate to, so
// threshold/cooldown exhaustion is moot for a lone candidate.
func TestRotator_Pick_SingleAccountNoOp(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	only := withUsage(acct("only"), 99)
	got, err := r.Pick("prov", "only", []Account{only})
	require.NoError(t, err)
	require.Equal(t, "only", got.ID)
}

// TestRotator_Pick_SingleDisabledAccountIsExhausted is the regression test
// for the sole-account fast path bypassing the Disabled check entirely:
// Disabled means "skipped by rotation and selection" (see Account's doc
// comment) regardless of how many candidates exist, so a disabled lone
// account must report ErrAllExhausted, not hand back credentials the user
// turned off.
func TestRotator_Pick_SingleDisabledAccountIsExhausted(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	only := acct("only")
	only.Disabled = true
	_, err := r.Pick("prov", "only", []Account{only})
	require.Error(t, err)
	var exhausted *ErrAllExhausted
	require.True(t, errors.As(err, &exhausted))
}

// TestRotator_Pick_ZeroAccountsNoOp: no accounts at all is a clear error,
// not a panic or a zero-value success.
func TestRotator_Pick_ZeroAccountsNoOp(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	_, err := r.Pick("prov", "", nil)
	require.Error(t, err)
	var exhausted *ErrAllExhausted
	require.True(t, errors.As(err, &exhausted))
}

// TestRotator_Pick_UnknownUsageIsUsable: Pick must select an account with
// no usage snapshot rather than treating the missing data as exhausted.
func TestRotator_Pick_UnknownUsageIsUsable(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	exhausted := withUsage(acct("a1"), 95)
	fresh := acct("a2") // no usage recorded yet
	got, err := r.Pick("prov", "a1", []Account{exhausted, fresh})
	require.NoError(t, err)
	require.Equal(t, "a2", got.ID)
}

// TestRotator_Pick_SkipsDisabled confirms a Disabled account, even if its
// usage looks fine, is never picked.
func TestRotator_Pick_SkipsDisabled(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	disabled := acct("a1")
	disabled.Disabled = true
	ok := acct("a2")
	got, err := r.Pick("prov", "a1", []Account{disabled, ok})
	require.NoError(t, err)
	require.Equal(t, "a2", got.ID)
}

// TestRotator_Pick_SkipsCoolingDown confirms MarkRateLimited actually
// removes an account from consideration until its cooldown expires.
func TestRotator_Pick_SkipsCoolingDown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := NewRotator(RotationPolicy{}, WithClock(func() time.Time { return now }))
	a1 := acct("a1")
	a2 := acct("a2")
	r.MarkRateLimited("a1", 5*time.Minute)

	got, err := r.Pick("prov", "a1", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a2", got.ID)

	// Once the cooldown has passed, a1 is usable again.
	now = now.Add(6 * time.Minute)
	got, err = r.Pick("prov", "a2", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a1", got.ID)
}

// TestRotator_Pick_OrderDoesNotDropUnnamedAccounts: an account missing
// from RotationPolicy.Order must still be reachable — Order reorders
// known accounts, it never hides accounts it doesn't mention.
func TestRotator_Pick_OrderDoesNotDropUnnamedAccounts(t *testing.T) {
	t.Parallel()
	// Order only names "a1", which is disabled, so Pick must fall through
	// to "a2" even though Order never mentions it.
	r := NewRotator(RotationPolicy{Order: []string{"a1"}})
	a1 := acct("a1")
	a1.Disabled = true
	a2 := acct("a2")
	got, err := r.Pick("prov", "a1", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a2", got.ID)
}

// TestRotator_Pick_OrderPreference confirms Order actually reorders when
// more than one candidate is usable: the configured order wins over
// store order.
func TestRotator_Pick_OrderPreference(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{Order: []string{"a2", "a1"}})
	a1 := acct("a1")
	a2 := acct("a2")
	// Store order is a1, a2; configured order prefers a2.
	got, err := r.Pick("prov", "a1", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a2", got.ID)
}

// TestRotator_Pick_ExhaustedReturnsEarliestReset covers decision Р4: when
// nothing is usable, the error must carry the EARLIEST reset among the
// candidates, not the latest, not an arbitrary one.
func TestRotator_Pick_ExhaustedReturnsEarliestReset(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := NewRotator(RotationPolicy{})

	a1 := acct("a1")
	a1.Usage = Usage{Primary: UsageWindow{UsedPercent: 95, WindowMinutes: 60, ResetsAt: base.Add(2 * time.Hour)}}
	a2 := acct("a2")
	a2.Usage = Usage{Primary: UsageWindow{UsedPercent: 95, WindowMinutes: 60, ResetsAt: base.Add(30 * time.Minute)}}

	_, err := r.Pick("prov", "a1", []Account{a1, a2})
	require.Error(t, err)
	var exhausted *ErrAllExhausted
	require.True(t, errors.As(err, &exhausted))
	require.True(t, exhausted.ResetsAt.Equal(base.Add(30*time.Minute)),
		"want earliest reset %s, got %s", base.Add(30*time.Minute), exhausted.ResetsAt)
}

// TestRotator_Pick_ExhaustedIgnoresDisabledAccountReset is the regression
// test for G24: a disabled account's reset time used to be counted toward
// ErrAllExhausted.ResetsAt even though it can never be picked (usableLocked
// rejects Disabled unconditionally). A user who disabled an account would
// see "resets at 15:04" for a moment nothing actually changes at, while the
// account that will genuinely become usable resets later.
func TestRotator_Pick_ExhaustedIgnoresDisabledAccountReset(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := NewRotator(RotationPolicy{})

	// a1 is disabled and would reset first, but must never be reflected in
	// ResetsAt - it stays exhausted forever regardless of the clock.
	a1 := acct("a1")
	a1.Disabled = true
	a1.Usage = Usage{Primary: UsageWindow{UsedPercent: 95, WindowMinutes: 60, ResetsAt: base.Add(15 * time.Minute)}}
	// a2 is merely rate-limited and resets later; its reset is the only one
	// that matters, since it is the account that will actually become
	// pickable again.
	a2 := acct("a2")
	a2.Usage = Usage{Primary: UsageWindow{UsedPercent: 95, WindowMinutes: 60, ResetsAt: base.Add(2 * time.Hour)}}

	_, err := r.Pick("prov", "a1", []Account{a1, a2})
	require.Error(t, err)
	var exhausted *ErrAllExhausted
	require.True(t, errors.As(err, &exhausted))
	require.True(t, exhausted.ResetsAt.Equal(base.Add(2*time.Hour)),
		"want the enabled account's reset %s, got %s (the disabled account's earlier reset must be ignored)",
		base.Add(2*time.Hour), exhausted.ResetsAt)
}

// TestRotator_Pick_ExhaustedZeroResetWhenUnknown: when no candidate
// carries a known reset time, ResetsAt must stay the zero time rather
// than fabricating one.
func TestRotator_Pick_ExhaustedZeroResetWhenUnknown(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	a1 := withUsage(acct("a1"), 95) // no ResetsAt set
	a2 := withUsage(acct("a2"), 95)
	_, err := r.Pick("prov", "a1", []Account{a1, a2})
	require.Error(t, err)
	var exhausted *ErrAllExhausted
	require.True(t, errors.As(err, &exhausted))
	require.True(t, exhausted.ResetsAt.IsZero())
}

// TestErrAllExhausted_Error confirms the message is readable and
// includes the reset time when known, and omits it when not.
func TestErrAllExhausted_Error(t *testing.T) {
	t.Parallel()
	reset := time.Date(2026, 1, 1, 14, 30, 0, 0, time.UTC)
	withReset := &ErrAllExhausted{ProviderID: "codex", ResetsAt: reset}
	require.Contains(t, withReset.Error(), "codex")
	require.Contains(t, withReset.Error(), "14:30")

	noReset := &ErrAllExhausted{ProviderID: "codex"}
	require.Contains(t, noReset.Error(), "codex")
	require.NotContains(t, noReset.Error(), "00:00")
}

// TestRotator_Pick_Debounce is the
// sharper debounce test: force order to prefer a1 on the second call
// (simulating a config change mid-flight) while a2 — the currently
// active account — is still usable. Within the debounce window, Pick
// must stay on a2 rather than hopping back to a1.
func TestRotator_Pick_Debounce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := NewRotator(RotationPolicy{Order: []string{"a2", "a1"}},
		WithClock(func() time.Time { return now }),
		WithDebounce(time.Minute))

	a1 := acct("a1")
	a2 := acct("a2")

	got, err := r.Pick("prov", "a1", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a2", got.ID)

	// Reconfigure to prefer a1 and re-pick well inside the debounce
	// window, with a2 (current) still perfectly usable.
	r2 := NewRotator(RotationPolicy{Order: []string{"a1", "a2"}},
		WithClock(func() time.Time { return now }),
		WithDebounce(time.Minute))
	r2.lastRotation = now // simulate "just rotated" without a second Pick call
	now = now.Add(10 * time.Second)
	got, err = r2.Pick("prov", "a2", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a2", got.ID, "debounce should hold on the still-usable current account")

	// After the debounce window elapses, the preferred order takes over.
	now = now.Add(time.Minute)
	got, err = r2.Pick("prov", "a2", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a1", got.ID)
}

// TestRotator_Pick_ConcurrentUse exercises the concurrency requirement:
// many goroutines calling Pick and MarkRateLimited at once must not race
// (run with -race) and every call must return a valid result.
func TestRotator_Pick_ConcurrentUse(t *testing.T) {
	t.Parallel()
	r := NewRotator(RotationPolicy{})
	accs := []Account{acct("a1"), acct("a2"), acct("a3")}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.Pick("prov", "a1", accs)
		}()
		go func() {
			defer wg.Done()
			r.MarkRateLimited("a2", time.Minute)
		}()
	}
	wg.Wait()
}

// TestRotator_MarkRateLimited_FallsBackToConfiguredCooldown: a zero
// retryAfter (no Retry-After header) must fall back to
// RotationPolicy.Cooldown, not leave the account uncooled or cool it for
// the wrong duration.
func TestRotator_MarkRateLimited_FallsBackToConfiguredCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := NewRotator(RotationPolicy{Cooldown: 5 * time.Minute},
		WithClock(func() time.Time { return now }), WithDebounce(0))
	a1 := acct("a1")
	a2 := acct("a2")
	r.MarkRateLimited("a1", 0)

	// Still cooling down just before the configured 5 minutes elapse.
	now = now.Add(4*time.Minute + 59*time.Second)
	got, err := r.Pick("prov", "a1", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a2", got.ID)

	// Usable again once the configured cooldown has fully elapsed.
	now = now.Add(2 * time.Second)
	got, err = r.Pick("prov", "a2", []Account{a1, a2})
	require.NoError(t, err)
	require.Equal(t, "a1", got.ID)
}
