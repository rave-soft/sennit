package agent

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	"github.com/stretchr/testify/require"
)

// plusUsage is what a Plus account reports: a short window and a weekly
// one. proUsage is the other shape the backend produces - a weekly window
// only, with the short one absent rather than merely unused.
func plusUsage(shortUsed, weeklyUsed int, shortReset, weeklyReset time.Time) codex.Usage {
	return codex.Usage{
		Plan:      "plus",
		Primary:   codex.UsageWindow{UsedPercent: shortUsed, WindowMinutes: 300, ResetsAt: shortReset},
		Secondary: codex.UsageWindow{UsedPercent: weeklyUsed, WindowMinutes: 10080, ResetsAt: weeklyReset},
	}
}

func proUsage(weeklyUsed int, weeklyReset time.Time) codex.Usage {
	return codex.Usage{
		Plan:    "pro",
		Primary: codex.UsageWindow{UsedPercent: weeklyUsed, WindowMinutes: 10080, ResetsAt: weeklyReset},
	}
}

func TestSpentWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	in := func(d time.Duration) time.Time { return now.Add(d) }

	t.Run("plus: the spent short window is the one reported", func(t *testing.T) {
		t.Parallel()
		w, ok := spentWindow(plusUsage(100, 40, in(2*time.Hour), in(80*time.Hour)), now)
		require.True(t, ok)
		require.Equal(t, 300, w.WindowMinutes)
		require.Equal(t, in(2*time.Hour), w.ResetsAt)
	})

	t.Run("plus: the weekly window wins when it is the fuller one", func(t *testing.T) {
		t.Parallel()
		w, ok := spentWindow(plusUsage(96, 100, in(2*time.Hour), in(80*time.Hour)), now)
		require.True(t, ok)
		require.Equal(t, 10080, w.WindowMinutes)
	})

	t.Run("pro: a plan with only a weekly window is reported on it", func(t *testing.T) {
		t.Parallel()
		w, ok := spentWindow(proUsage(100, in(50*time.Hour)), now)
		require.True(t, ok)
		require.Equal(t, 10080, w.WindowMinutes)
	})

	t.Run("a window with room left explains no 429", func(t *testing.T) {
		t.Parallel()
		_, ok := spentWindow(plusUsage(40, 30, in(2*time.Hour), in(80*time.Hour)), now)
		require.False(t, ok)
	})

	t.Run("a reset already past is no reset to quote", func(t *testing.T) {
		t.Parallel()
		_, ok := spentWindow(plusUsage(100, 100, in(-time.Minute), in(-time.Hour)), now)
		require.False(t, ok)
	})

	t.Run("no snapshot at all", func(t *testing.T) {
		t.Parallel()
		_, ok := spentWindow(codex.Usage{}, now)
		require.False(t, ok)
	})
}

// TestProviderLimitError_UnwrapsBoth is what makes the whole path work:
// the retry loop must see fantasy.ErrStopRetrying, and every existing
// errors.As on *fantasy.ProviderError must still find the original 429.
func TestProviderLimitError_UnwrapsBoth(t *testing.T) {
	t.Parallel()

	original := &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "The usage limit has been reached"}
	limit := providerLimitFromUsage(
		config.ProviderConfig{ID: "codex", Name: "Codex"},
		plusUsage(100, 40, time.Now().Add(time.Hour), time.Now().Add(80*time.Hour)),
		original,
		time.Now(),
	)
	require.NotNil(t, limit)

	var err error = limit
	require.True(t, errors.Is(err, fantasy.ErrStopRetrying))
	var providerErr *fantasy.ProviderError
	require.True(t, errors.As(err, &providerErr))
	require.Equal(t, original, providerErr)
}

func TestFormatWaitUntil(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	require.Equal(t, "4m", formatWaitUntil(now.Add(4*time.Minute+30*time.Second), now))
	require.Equal(t, "1m", formatWaitUntil(now.Add(20*time.Second), now), "a wait under a minute is still a wait")
	require.Equal(t, "1h 30m", formatWaitUntil(now.Add(90*time.Minute), now))
	require.Equal(t, "3d 8h", formatWaitUntil(now.Add(80*time.Hour), now))
	require.Empty(t, formatWaitUntil(now.Add(-time.Minute), now), "a reset already past is not a wait")
}

func TestProviderLimitError_Message(t *testing.T) {
	t.Parallel()

	resets := time.Now().Add(90 * time.Minute)
	limit := &ProviderLimitError{
		Provider: "codex", ProviderName: "Codex", Plan: "plus",
		WindowMinutes: 300, UsedPercent: 100, ResetsAt: resets,
	}
	msg := limit.Error()
	require.Contains(t, msg, "Codex plus")
	require.Contains(t, msg, "5h limit")
	require.Contains(t, msg, "resets at "+resets.Format("15:04"))
	require.Contains(t, msg, "(in 1h ", "the countdown truncates, so a wait quoted is never longer than the real one")

	all := &ProviderLimitError{Provider: "codex", ProviderName: "Codex", ResetsAt: resets, AllAccounts: true}
	require.Contains(t, all.Error(), "every account is out of allowance")
}

// TestRateLimitCooldown covers the rule that keeps rotation away from an
// account whose window resets in days: the header wins when present, the
// spent window is used when it is not, and neither known means "let the
// policy decide" (zero).
func TestRateLimitCooldown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	usage := plusUsage(100, 40, now.Add(3*time.Hour), now.Add(80*time.Hour))

	withHeader := &fantasy.ProviderError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseHeaders: map[string]string{"retry-after": "42"},
	}
	require.Equal(t, 42*time.Second, rateLimitCooldown(withHeader, usage, now))

	bare := &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}
	require.Equal(t, 3*time.Hour+resumeGrace, rateLimitCooldown(bare, usage, now))

	require.Zero(t, rateLimitCooldown(bare, codex.Usage{}, now))
}

// codexLimitBuilder wires the smallest runtimeBuilder the rate-limit
// callback needs for a Codex provider: the store it lists accounts from
// and the usage lookup it reads windows through.
func codexLimitBuilder(t *testing.T, providerCfg config.ProviderConfig, accs ...accounts.Account) *runtimeBuilder {
	t.Helper()
	cfg := testRotationConfigStoreWithProvider(t, providerCfg)
	effective, err := providerruntime.FromConfig(providerCfg, cfg.Config().RuntimeResolver())
	require.NoError(t, err)
	cfg.Config().SetRuntimeProvider(codex.ProviderID, effective)
	return &runtimeBuilder{
		agentDeps: &agentDeps{
			cfg:           cfg,
			notify:        &recordingNotifier{},
			accountsStore: codexAccountStore(accs...),
			codexUsage:    codex.UsageFor,
		},
		runtime: newRuntimeCache(),
	}
}

// TestRateLimitCallback_SpentWindowStopsTheTurn is the reported case: one
// Codex account, its 5h window spent, a 429 in hand. The callback must
// report the limit - with the reset time the headers already gave - rather
// than send the turn back round the retry loop.
func TestRateLimitCallback_SpentWindowStopsTheTurn(t *testing.T) {
	resets := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	codex.RecordUsageFor("acct-spent", codex.Usage{
		Plan:      "plus",
		Primary:   codex.UsageWindow{UsedPercent: 100, WindowMinutes: 300, ResetsAt: resets},
		Secondary: codex.UsageWindow{UsedPercent: 61, WindowMinutes: 10080, ResetsAt: time.Now().Add(80 * time.Hour)},
	})

	providerCfg := codexProviderConfig("acct-spent", true)
	b := codexLimitBuilder(t, providerCfg, apiKeyAccount("acct-spent", "key-a"))
	cred, ok := b.cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)

	cb := b.makeRateLimitCallback(providerCfg, cred, nil, runtimeOperationPort{})
	require.NotNil(t, cb)

	err := cb(t.Context(), rateLimitErr(nil))
	var limit *ProviderLimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, resets, limit.ResetsAt)
	require.Equal(t, 300, limit.WindowMinutes)
	require.Equal(t, "plus", limit.Plan)
	require.ErrorIs(t, err, fantasy.ErrStopRetrying, "the retry pass must stop on a limit that outlasts it")
}

// TestRateLimitCallback_WeeklyOnlyPlan is the Pro shape: no short window
// at all, so the weekly one is what the limit is reported on.
func TestRateLimitCallback_WeeklyOnlyPlan(t *testing.T) {
	resets := time.Now().Add(50 * time.Hour).Truncate(time.Second)
	codex.RecordUsageFor("acct-pro", codex.Usage{
		Plan:    "pro",
		Primary: codex.UsageWindow{UsedPercent: 100, WindowMinutes: 10080, ResetsAt: resets},
	})

	providerCfg := codexProviderConfig("acct-pro", true)
	b := codexLimitBuilder(t, providerCfg, apiKeyAccount("acct-pro", "key-a"))
	cred, ok := b.cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)

	err := b.makeRateLimitCallback(providerCfg, cred, nil, runtimeOperationPort{})(t.Context(), rateLimitErr(nil))
	var limit *ProviderLimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, 10080, limit.WindowMinutes)
	require.Equal(t, resets, limit.ResetsAt)
	require.Contains(t, limit.Error(), "weekly limit")
}

// TestRateLimitCallback_RotationOffStillReportsTheLimit covers the second
// surface: rotation disabled for the provider used to mean no hook at all,
// so a single-account user - the common case - saw the bare 429 and three
// pointless retries.
func TestRateLimitCallback_RotationOffStillReportsTheLimit(t *testing.T) {
	resets := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	codex.RecordUsageFor("acct-norotate", codex.Usage{
		Plan:    "plus",
		Primary: codex.UsageWindow{UsedPercent: 100, WindowMinutes: 300, ResetsAt: resets},
	})

	providerCfg := codexProviderConfig("acct-norotate", false)
	b := codexLimitBuilder(t, providerCfg, apiKeyAccount("acct-norotate", "key-a"))
	cred, ok := b.cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)

	cb := b.makeRateLimitCallback(providerCfg, cred, nil, runtimeOperationPort{})
	require.NotNil(t, cb, "a provider that reports usage gets the hook even with rotation off")

	err := cb(t.Context(), rateLimitErr(nil))
	var limit *ProviderLimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, resets, limit.ResetsAt)
}

// TestRateLimitCallback_BurstKeepsRetrying is the guard on the other side:
// a 429 with allowance still left is not a subscription limit, so the
// callback must leave the retry pass alone.
func TestRateLimitCallback_BurstKeepsRetrying(t *testing.T) {
	codex.RecordUsageFor("acct-burst", codex.Usage{
		Plan:    "plus",
		Primary: codex.UsageWindow{UsedPercent: 20, WindowMinutes: 300, ResetsAt: time.Now().Add(time.Hour)},
	})

	providerCfg := codexProviderConfig("acct-burst", false)
	b := codexLimitBuilder(t, providerCfg, apiKeyAccount("acct-burst", "key-a"))
	cred, ok := b.cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)

	err := b.makeRateLimitCallback(providerCfg, cred, nil, runtimeOperationPort{})(t.Context(), rateLimitErr(nil))
	require.Error(t, err)
	var limit *ProviderLimitError
	require.False(t, errors.As(err, &limit))
	require.NotErrorIs(t, err, fantasy.ErrStopRetrying)
}
