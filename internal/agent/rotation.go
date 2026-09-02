package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// rotatorFor returns providerCfg's Rotator, building it on first use, or
// nil when rotation is disabled for this provider (no Rotation config, or
// Rotation.Enabled false).
//
// This is the single switch that makes rotation a complete no-op when
// disabled: every rotation call site (makeThresholdRotateCallback,
// makeRateLimitCallback) starts here and returns nil itself as soon as
// this does, so a disabled provider never gets a Rotator constructed, never
// consults accStore, and never wires an OnRateLimit/RotateThreshold
// callback onto a call at all - behavior is provably identical to before
// rotation existed, not merely "happens to be a no-op" once invoked.
func (b *runtimeBuilder) rotatorFor(providerCfg config.ProviderConfig) *accounts.Rotator {
	if providerCfg.Rotation == nil || !providerCfg.Rotation.Enabled {
		return nil
	}
	b.rotatorsMu.Lock()
	defer b.rotatorsMu.Unlock()
	if r, ok := b.rotators[providerCfg.ID]; ok {
		return r
	}
	if b.rotators == nil {
		b.rotators = make(map[string]*accounts.Rotator)
	}
	r := accounts.NewRotator(providerCfg.Rotation.ToPolicy())
	b.rotators[providerCfg.ID] = r
	return r
}

// currentRotationAccount resolves the account a rotation callback should
// act on for providerCfg's provider. providerCfg is captured by value once
// per turn (turn_dispatcher.go), so after the first rotation it still names
// the pre-rotation account; active, if present, is restored on every
// successful applyRotationPick with a freshly rebuilt runtime whose
// providerCfg carries the account that is actually live now. Falling back
// to providerCfg.Account keeps this correct for callers that pass a nil
// active (e.g. no top-level agent to rebuild for).
//
// active is shared with makeAuthRefreshCallback, which stores whatever
// runtimeFor built for the CURRENT config - not necessarily this provider.
// If the user switches the main model to a different provider mid-turn and
// a 401 refresh runs on it while this provider is still streaming (a
// sub-agent on a second provider, say), active now describes that other
// provider. Trusting its Account blindly would mark/rotate an account this
// provider's Rotator has never heard of. Only adopt the loaded runtime's
// account when it was actually built for providerCfg; otherwise fall back
// to the captured value exactly as when active is nil.
func currentRotationAccount(providerCfg config.ProviderConfig, active *activeRuntime) string {
	if active != nil {
		if runtime := active.load(); runtime != nil && runtime.providerCfg.ID == providerCfg.ID {
			return runtime.providerCfg.Account
		}
	}
	return providerCfg.Account
}

// accountLabel returns a's display name for a rotation notification:
// its user-editable Label when set, its bookkeeping ID otherwise.
func accountLabel(a accounts.Account) string {
	if a.Label != "" {
		return a.Label
	}
	return a.ID
}

// worstKnownRemainingPercent returns 100 minus the highest UsedPercent
// among a's known usage windows - the remaining allowance on whichever
// window is closest to exhausted, which is the one that actually tripped
// ShouldRotate. Returns -1 when neither window is known (nothing to
// report; callers omit the percent from their message in that case).
func worstKnownRemainingPercent(u accounts.Usage) int {
	worst := -1
	for _, w := range []accounts.UsageWindow{u.Primary, u.Secondary} {
		if w.Known() && w.UsedPercent > worst {
			worst = w.UsedPercent
		}
	}
	if worst < 0 {
		return -1
	}
	return 100 - worst
}

// applyRotationPick activates picked as providerID's active account and,
// when active is non-nil, rebuilds and stores the runtime so the next
// request actually uses the new credentials - the same two steps
// makeAuthRefreshCallback takes after a successful credential refresh
// (plan §5.2: activation is projected into the live ProviderConfig, never
// touching the provider build path itself).
func (b *runtimeBuilder) applyRotationPick(ctx context.Context, providerID string, picked accounts.Account, active *activeRuntime, inputs runtimeToolInputs) error {
	if err := b.cfg.ActivateAccount(config.ScopeGlobal, providerID, picked); err != nil {
		return fmt.Errorf("activating rotated account %s for provider %s: %w", picked.ID, providerID, err)
	}
	if active == nil {
		return nil
	}
	runtime, err := b.runtimeFor(ctx, inputs)
	if err != nil {
		return fmt.Errorf("rebuilding runtime after rotating provider %s to account %s: %w", providerID, picked.ID, err)
	}
	active.store(runtime)
	return nil
}

// makeThresholdRotateCallback returns the RotateThreshold hook (plan
// §5.5's proactive trigger, Codex today): called once per finished step,
// it checks the active account's last usage snapshot and, if
// accounts.Rotator.ShouldRotate says the account is over threshold,
// switches to the next usable one.
//
// Returns nil - meaning "nothing to do here, ever" - when rotation is
// disabled for providerCfg (rotatorFor's nil check) or the provider isn't
// a RotateThreshold one, so a RotateRateLimit or RotateNever provider
// never even gets this hook wired onto a call.
//
// The returned function never fails the turn: every error path logs and
// returns, exactly matching what happens today when a request simply runs
// over quota on a single-account setup - the user keeps using the current
// (over-threshold) account rather than losing the step's own result over
// a rotation that didn't work out.
func (b *runtimeBuilder) makeThresholdRotateCallback(providerCfg config.ProviderConfig, active *activeRuntime, port runtimeOperationPort) func(context.Context) {
	rotator := b.rotatorFor(providerCfg)
	if rotator == nil || accounts.CapabilitiesOf(providerCfg.ID).RotateOn != accounts.RotateThreshold {
		return nil
	}
	inputs := port.inputs
	return func(ctx context.Context) {
		// Resolve the account live rather than trusting providerCfg.Account:
		// providerCfg is captured by value once per turn, so after a
		// rotation it still names the pre-rotation account (see
		// currentRotationAccount's doc comment).
		account := currentRotationAccount(providerCfg, active)
		// RotateThreshold is Codex-only today (see capabilities.go), so
		// reading its usage snapshot through the injected codexUsage
		// lookup (production: codex.UsageFor) is deliberate, not a
		// layering slip - a future non-Codex threshold provider would
		// need this coupling broken out (e.g. a small per-provider
		// usage-lookup registry) before it could reuse this path.
		if b.codexUsage == nil {
			return
		}
		usage, ok := b.codexUsage(account)
		if !ok {
			return
		}
		all, err := b.accStore.List(providerCfg.ID)
		if err != nil {
			slog.Warn("Threshold rotation: failed to list accounts", "provider", providerCfg.ID, "error", err)
			return
		}
		acct := accounts.Account{ID: account, Usage: usage.Snapshot()}
		for i, a := range all {
			if a.ID == account {
				acct = a
				acct.Usage = usage.Snapshot()
				// Pick reads exhaustion off its own candidates list, not
				// off acct separately (unlike ShouldRotate, which
				// takes acct directly) - without this, Pick would see
				// the store's possibly-stale Usage for the active
				// account and, finding it "unknown" rather than
				// exhausted, could pick the very account this callback
				// is trying to rotate away from.
				all[i] = acct
				break
			}
		}
		if !rotator.ShouldRotate(acct, all) {
			return
		}
		picked, err := rotator.Pick(providerCfg.ID, acct.ID, all)
		if err != nil {
			slog.Warn("Threshold rotation: no usable account", "provider", providerCfg.ID, "error", err)
			return
		}
		if picked.ID == acct.ID {
			return
		}
		if err := b.applyRotationPick(ctx, providerCfg.ID, picked, active, inputs); err != nil {
			slog.Warn("Threshold rotation: failed to apply picked account", "provider", providerCfg.ID, "error", err)
			return
		}
		if b.notify != nil {
			remaining := worstKnownRemainingPercent(acct.Usage)
			msg := fmt.Sprintf("%s: switched to %q", providerCfg.Name, accountLabel(picked))
			if remaining >= 0 {
				msg = fmt.Sprintf("%s, %q had %d%% left", msg, accountLabel(acct), remaining)
			}
			b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
				Type:       notify.TypeAccountRotated,
				ProviderID: providerCfg.ID,
				Message:    msg,
			})
		}
	}
}

// makeRateLimitCallback returns the fantasy OnRateLimitFunc for the
// reactive rotation trigger (plan §5.5, every RotateRateLimit provider):
// on a 429, it marks the active account cooling down, picks the next
// usable one via the provider's Rotator, and applies it exactly like
// makeThresholdRotateCallback (§5.2 projection + runtime rebuild).
//
// Returns nil when rotation is disabled for providerCfg or the provider
// isn't a RotateRateLimit one, mirroring makeAuthRefreshCallback's own
// "no mechanism configured" nil return - fantasy never engages an unset
// hook, so a disabled/non-matching provider's retry behavior is untouched.
//
// On success, the returned function returns nil so fantasy retries
// immediately with the new account's credentials (RetryOptions.OnRateLimit's
// contract). When every candidate is exhausted, it returns the
// *accounts.ErrAllExhausted from Pick unchanged, which RetryOptions.OnRateLimit
// treats as "rotation didn't help" - normal backoff resumes and the
// ORIGINAL 429 (not this error) is what a caller ultimately sees; see
// RetryWithExponentialBackoffRespectingRetryHeaders and runTurn.handleStreamError.
func (b *runtimeBuilder) makeRateLimitCallback(providerCfg config.ProviderConfig, active *activeRuntime, port runtimeOperationPort) fantasy.OnRateLimitFunc {
	rotator := b.rotatorFor(providerCfg)
	if rotator == nil || accounts.CapabilitiesOf(providerCfg.ID).RotateOn != accounts.RotateRateLimit {
		return nil
	}
	inputs := port.inputs
	return func(ctx context.Context, providerErr *fantasy.ProviderError) error {
		// Resolve the account live rather than trusting providerCfg.Account:
		// providerCfg is captured by value once per turn, so after a
		// rotation it still names the pre-rotation account (see
		// currentRotationAccount's doc comment) - without this, a second
		// 429 on the newly-picked account would mark the WRONG account
		// rate-limited and hot-loop retrying on the still-limited one.
		account := currentRotationAccount(providerCfg, active)
		rotator.MarkRateLimited(account, retryAfterFromHeaders(providerErr))

		all, err := b.accStore.List(providerCfg.ID)
		if err != nil {
			slog.Warn("Rate-limit rotation: failed to list accounts", "provider", providerCfg.ID, "error", err)
			return err
		}
		picked, err := rotator.Pick(providerCfg.ID, account, all)
		if err != nil {
			var exhausted *accounts.ErrAllExhausted
			if errors.As(err, &exhausted) && b.notify != nil {
				msg := fmt.Sprintf("%s: all accounts exhausted", providerCfg.Name)
				if !exhausted.ResetsAt.IsZero() {
					msg = fmt.Sprintf("%s, resets at %s", msg, exhausted.ResetsAt.Format("15:04"))
				}
				b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
					Type:       notify.TypeAccountRotationExhausted,
					ProviderID: providerCfg.ID,
					Message:    msg,
				})
			}
			return err
		}
		if picked.ID == account {
			// Pick found nothing better to switch to - most commonly a
			// single-account provider, where Pick's one-candidate fast
			// path hands back the very account MarkRateLimited just put
			// on cooldown without even consulting it. Applying picked
			// would be a no-op ActivateAccount call for no reason. But
			// returning nil here would tell fantasy "credentials
			// rotated, retry immediately" (RetryOptions.OnRateLimit),
			// which fires the very next attempt at the still-limited
			// account with no delay at all, burning a retry for
			// nothing. Return ErrAllExhausted instead - Pick's own
			// verdict for "no usable account right now" - so
			// OnRateLimit's error path takes over and normal backoff
			// applies before the retry.
			return &accounts.ErrAllExhausted{ProviderID: providerCfg.ID}
		}
		if err := b.applyRotationPick(ctx, providerCfg.ID, picked, active, inputs); err != nil {
			slog.Warn("Rate-limit rotation: failed to apply picked account", "provider", providerCfg.ID, "error", err)
			return err
		}
		if b.notify != nil {
			b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
				Type:       notify.TypeAccountRotated,
				ProviderID: providerCfg.ID,
				Message:    fmt.Sprintf("%s: switched to %q after a rate limit", providerCfg.Name, accountLabel(picked)),
			})
		}
		return nil
	}
}

// retryAfterFromHeaders extracts the Retry-After delay from a
// *fantasy.ProviderError's response headers, for MarkRateLimited. This
// deliberately duplicates the couple of lines third_party/fantasy/retry.go's
// unexported getRetryDelayInMs already does (retry-after-ms, then
// Retry-After as seconds or an HTTP date) rather than exporting that
// helper across the vendor boundary for one small caller - see plan §9
// risk 6 on keeping the fork's surface area minimal.
func retryAfterFromHeaders(err *fantasy.ProviderError) time.Duration {
	if err == nil || err.ResponseHeaders == nil {
		return 0
	}
	h := err.ResponseHeaders
	if ms, ok := h["retry-after-ms"]; ok {
		if v, parseErr := strconv.ParseFloat(ms, 64); parseErr == nil {
			return time.Duration(v) * time.Millisecond
		}
	}
	if ra, ok := h["retry-after"]; ok {
		if secs, parseErr := strconv.ParseFloat(ra, 64); parseErr == nil {
			return time.Duration(secs) * time.Second
		}
		if t, parseErr := time.Parse(time.RFC1123, ra); parseErr == nil {
			return time.Until(t)
		}
	}
	return 0
}
