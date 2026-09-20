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
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
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
// consults accountsStore, and never wires an OnRateLimit/RotateThreshold
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
// act on for providerCfg's provider. cred is captured by value once per
// turn (turn_dispatcher.go), so after the first rotation it still names
// the pre-rotation account; active, if present, is restored on every
// successful applyRotationPick with a freshly rebuilt runtime whose
// providerCredentials carries the account that is actually live now.
// Falling back to cred.Account keeps this correct for callers that pass a
// nil active (e.g. no top-level agent to rebuild for).
//
// active is shared with makeAuthRefreshCallback, which stores whatever
// runtimeFor built for the CURRENT config - not necessarily this provider.
// If the user switches the main model to a different provider mid-turn and
// a 401 refresh runs on it while this provider is still streaming (a
// sub-agent on a second provider, say), active now describes that other
// provider. Trusting its Account blindly would mark/rotate an account this
// provider's Rotator has never heard of. Only adopt the loaded runtime's
// account when it was actually built for providerCfg (identity is compared
// on providerCfg.ID, which is identical to providerCredentials.ID — both
// are keyed by and set to the same provider id at build time); otherwise
// fall back to the captured value exactly as when active is nil.
func currentRotationAccount(providerCfg config.ProviderConfig, cred providerstate.Provider, active *activeRuntime) string {
	if active != nil {
		if runtime := active.load(); runtime != nil && runtime.providerCfg.ID == providerCfg.ID {
			return runtime.providerCredentials.Account
		}
	}
	return cred.Account
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

// runtimeRebuild recompiles the runtime active should carry after a
// rotation pick lands. It is the one thing that legitimately differs
// between a top-level turn and a delegation: applyRotationPick's decision
// half (activate the picked account, then rebuild) is identical for both,
// but WHAT gets rebuilt is not interchangeable.
//
// A top-level rebuild goes through runtimeFor(inputs) - the full
// coordinator runtime, complete with the named-agent roster and every
// coder-only tool. A delegation must rebuild through buildSubAgentRuntime
// instead, which recompiles only the model/provider pair. Passing the
// top-level rebuild for a delegation would hand a delegate the coder's own
// tools and system prompt on its very next request - exactly the privilege
// escalation delegation_finalizer.go's agentTool(allowNamedAgents=false)
// and buildSubAgentRuntime's own doc comment exist to prevent. Threading
// the strategy in as a parameter (rather than switching on some "is this a
// sub-agent" flag inside applyRotationPick) is what makes that mistake a
// compile-time impossibility instead of a runtime one: each caller can only
// ever supply the rebuild that matches how it obtained providerCfg/cred in
// the first place.
type runtimeRebuild func(ctx context.Context) (*compiledRuntime, error)

// applyRotationPick activates picked as providerID's active account and,
// when active is non-nil, rebuilds and stores the runtime via rebuild so
// the next request actually uses the new credentials - the same two steps
// makeAuthRefreshCallback takes after a successful credential refresh:
// activation is projected into the live ProviderConfig, never touching
// the provider build path itself.
func (b *runtimeBuilder) applyRotationPick(ctx context.Context, providerID string, picked accounts.Account, active *activeRuntime, rebuild runtimeRebuild) error {
	providerCfg, ok := b.cfg.Config().Providers.Get(providerID)
	if !ok {
		return fmt.Errorf("provider %s not found after rotation", providerID)
	}
	if err := b.cfg.ActivateAccount(config.ScopeGlobal, providerID, picked); err != nil {
		return fmt.Errorf("activating rotated account %s for provider %s: %w", picked.ID, providerID, err)
	}
	if active == nil {
		return nil
	}
	runtime, err := rebuild(ctx)
	if err != nil {
		// A 401 from a just-rotated account means the new credential is
		// invalid (bad token, expired, etc.). Mark it rate-limited so
		// Pick will never hand it back again in this rotation cycle,
		// preventing a hot loop that keeps activating the same broken
		// account. The original error is still returned so fantasy's
		// OnAuthRefresh path (or normal backoff) can engage as usual.
		if isAuthError(err) {
			if rotator := b.rotatorFor(providerCfg); rotator != nil {
				rotator.MarkRateLimited(picked.ID, 0)
			}
		}
		return fmt.Errorf("rebuilding runtime after rotating provider %s to account %s: %w", providerID, picked.ID, err)
	}
	active.store(runtime)
	return nil
}

// isAuthError reports whether err carries a *fantasy.ProviderError that
// looks like an authentication failure (401 or AuthError flag). It is
// checked after a rotation rebuild so an invalid new credential is
// quarantined rather than retried against.
func isAuthError(err error) bool {
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.StatusCode == 401 || pe.AuthError
}

// makeThresholdRotateCallback returns the RotateThreshold hook (the
// proactive rotation trigger): called once per finished step,
// it checks the active account's last usage snapshot and, if
// accounts.Rotator.ShouldRotate says the account is over threshold,
// switches to the next usable one.
//
// Returns nil - meaning "nothing to do here, ever" - when rotation is
// disabled for providerCfg (rotatorFor's nil check) or the provider does
// not rotate on a threshold, so a RotateRateLimit or RotateNever provider
// never even gets this hook wired onto a call.
//
// The returned function never fails the turn: every error path logs and
// returns, exactly matching what happens today when a request simply runs
// over quota on a single-account setup - the user keeps using the current
// (over-threshold) account rather than losing the step's own result over
// a rotation that didn't work out.
func (b *runtimeBuilder) makeThresholdRotateCallback(providerCfg config.ProviderConfig, cred providerstate.Provider, active *activeRuntime, port runtimeOperationPort) func(context.Context) {
	inputs := port.inputs
	return b.thresholdRotateCallback(providerCfg, cred, active, func(ctx context.Context) (*compiledRuntime, error) {
		return b.runtimeFor(ctx, inputs)
	})
}

// makeSubAgentThresholdRotateCallback is makeThresholdRotateCallback's
// delegation counterpart: same decision (mark, list, ShouldRotate, Pick),
// rebuilt through buildSubAgentRuntime(model) instead of runtimeFor(inputs)
// - see runtimeRebuild's doc comment for why the two must never be
// interchanged. model is the delegation's own resolved model (buildAgentModel
// in buildAgent), mirroring makeSubAgentAuthRefreshCallback.
func (b *runtimeBuilder) makeSubAgentThresholdRotateCallback(providerCfg config.ProviderConfig, cred providerstate.Provider, model Model, active *activeRuntime) func(context.Context) {
	return b.thresholdRotateCallback(providerCfg, cred, active, func(ctx context.Context) (*compiledRuntime, error) {
		return b.buildSubAgentRuntime(ctx, model)
	})
}

// thresholdRotateCallback holds the rotation decision both
// makeThresholdRotateCallback and makeSubAgentThresholdRotateCallback share;
// only the rebuild strategy differs between them (see runtimeRebuild).
func (b *runtimeBuilder) thresholdRotateCallback(providerCfg config.ProviderConfig, cred providerstate.Provider, active *activeRuntime, rebuild runtimeRebuild) func(context.Context) {
	rotator := b.rotatorFor(providerCfg)
	if rotator == nil || !accounts.CapabilitiesOf(providerCfg.ID).RotateOn.RotatesOnThreshold() {
		return nil
	}
	return func(ctx context.Context) {
		// Resolve the account live rather than trusting cred.Account:
		// cred is captured by value once per turn, so after a
		// rotation it still names the pre-rotation account (see
		// currentRotationAccount's doc comment).
		account := currentRotationAccount(providerCfg, cred, active)
		// RotateThreshold is Codex-only today (see capabilities.go), so
		// reading its usage snapshot through the injected codexUsage
		// lookup (production: codex.UsageFor) is deliberate, not a
		// layering slip - a future non-Codex threshold provider would
		// need this coupling broken out (e.g. a small per-provider
		// usage-lookup registry) before it could reuse this path.
		if b.codexUsage == nil {
			return
		}
		// The account list is read before the usage lookup because the
		// lookup needs it: the snapshot store is keyed by the provider's
		// own account id, not by Sennit's (see usageFor).
		all, err := b.accountsStore.List(providerCfg.ID)
		if err != nil {
			slog.Warn("Threshold rotation: failed to list accounts", "provider", providerCfg.ID, "error", err)
			return
		}
		usage, ok := b.usageFor(account, all)
		if !ok {
			return
		}
		all = b.freshenUsage(all)
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
		if err := b.applyRotationPick(ctx, providerCfg.ID, picked, active, rebuild); err != nil {
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
// reactive rotation trigger: on a 429, it marks the active account
// cooling down, picks the next usable one via the provider's Rotator,
// and applies it exactly like makeThresholdRotateCallback.
//
// Returns nil when rotation is disabled for providerCfg or the provider
// does not rotate on a rate limit, mirroring makeAuthRefreshCallback's
// own "no mechanism configured" nil return - fantasy never engages an
// unset hook, so a disabled/non-matching provider's retry behavior is
// untouched.
//
// On success, the returned function returns nil so fantasy retries
// immediately with the new account's credentials (RetryOptions.OnRateLimit's
// contract). When every candidate is exhausted, it returns the
// *accounts.ErrAllExhausted from Pick unchanged, which RetryOptions.OnRateLimit
// treats as "rotation didn't help" - normal backoff resumes and the
// ORIGINAL 429 (not this error) is what a caller ultimately sees; see
// RetryWithExponentialBackoffRespectingRetryHeaders and runTurn.handleStreamError.
func (b *runtimeBuilder) makeRateLimitCallback(providerCfg config.ProviderConfig, cred providerstate.Provider, active *activeRuntime, port runtimeOperationPort) fantasy.OnRateLimitFunc {
	inputs := port.inputs
	return b.rateLimitCallback(providerCfg, cred, active, func(ctx context.Context) (*compiledRuntime, error) {
		return b.runtimeFor(ctx, inputs)
	})
}

// makeSubAgentRateLimitCallback is makeRateLimitCallback's delegation
// counterpart: same decision (mark, list, Pick, apply-or-report-exhausted),
// rebuilt through buildSubAgentRuntime(model) instead of runtimeFor(inputs)
// - see runtimeRebuild's doc comment for why the two must never be
// interchanged. model is the delegation's own resolved model, mirroring
// makeSubAgentAuthRefreshCallback and makeSubAgentThresholdRotateCallback.
//
// Notifications are NOT suppressed for a delegation: a rotation or an
// exhaustion is exactly as true an event when it happens under a
// delegation as under the top-level turn (the account and its remaining
// budget are shared with every other caller of that provider), and
// swallowing them here would hide a delegation quietly burning through
// every configured account with nothing shown to the person running it.
// Parallel delegations rotating the same exhausted provider in quick
// succession can produce more than one notification in a short window,
// but that is the accurate account of what happened, not noise from this
// callback double-reporting a single event - see currentRotationAccount's
// per-call resolution, which keeps each delegation's report attributed to
// the account IT actually observed.
func (b *runtimeBuilder) makeSubAgentRateLimitCallback(providerCfg config.ProviderConfig, cred providerstate.Provider, model Model, active *activeRuntime) fantasy.OnRateLimitFunc {
	return b.rateLimitCallback(providerCfg, cred, active, func(ctx context.Context) (*compiledRuntime, error) {
		return b.buildSubAgentRuntime(ctx, model)
	})
}

// rateLimitCallback holds the rotation decision both makeRateLimitCallback
// and makeSubAgentRateLimitCallback share; only the rebuild strategy
// differs between them (see runtimeRebuild).
//
// It is wired for two overlapping reasons, and returns nil only when
// neither applies. Rotation is the first: a 429 marks the account and
// moves the turn to the next usable one. Usage reporting is the second: a
// provider that quotes its own windows (Codex) can tell a spent
// subscription window from a passing burst, and a spent window is worth
// ending the turn over instead of spending three backoff attempts on a
// refusal that stands for hours. A provider with neither gets no hook at
// all, exactly as before.
func (b *runtimeBuilder) rateLimitCallback(providerCfg config.ProviderConfig, cred providerstate.Provider, active *activeRuntime, rebuild runtimeRebuild) fantasy.OnRateLimitFunc {
	caps := accounts.CapabilitiesOf(providerCfg.ID)
	rotator := b.rotatorFor(providerCfg)
	rotates := rotator != nil && caps.RotateOn.RotatesOnRateLimit()
	reportsUsage := caps.Usage && b.codexUsage != nil
	if !rotates && !reportsUsage {
		return nil
	}
	return func(ctx context.Context, providerErr *fantasy.ProviderError) error {
		// Resolve the account live rather than trusting cred.Account:
		// cred is captured by value once per turn, so after a
		// rotation it still names the pre-rotation account (see
		// currentRotationAccount's doc comment) - without this, a second
		// 429 on the newly-picked account would mark the WRONG account
		// rate-limited and hot-loop retrying on the still-limited one.
		account := currentRotationAccount(providerCfg, cred, active)
		all, err := b.accountsStore.List(providerCfg.ID)
		if err != nil {
			slog.Warn("Rate-limit rotation: failed to list accounts", "provider", providerCfg.ID, "error", err)
			if !rotates {
				return providerErr
			}
			return err
		}
		usage, _ := b.usageFor(account, all)
		all = b.freshenUsage(all)
		if !rotates {
			// No rotation configured for this provider: the only thing
			// left to decide is whether the refusal is worth retrying.
			if limit := providerLimitFromUsage(providerCfg, usage, providerErr, time.Now()); limit != nil {
				return limit
			}
			return providerErr
		}
		rotator.MarkRateLimited(account, rateLimitCooldown(providerErr, usage, time.Now()))
		picked, err := rotator.Pick(providerCfg.ID, account, all)
		if err != nil {
			var exhausted *accounts.ErrAllExhausted
			if errors.As(err, &exhausted) {
				if b.notify != nil {
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
				// Every account is spent and Pick knows the earliest
				// any of them comes back: that is a limit the turn
				// should be ended on, not retried through.
				if limit := providerLimitExhausted(providerCfg, usage, exhausted, providerErr); limit != nil {
					return limit
				}
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
			// nothing. Return the limit this account is under when it
			// is known, and Pick's own verdict for "no usable account
			// right now" otherwise - either way OnRateLimit's error
			// path takes over instead of an immediate retry.
			if limit := providerLimitFromUsage(providerCfg, usage, providerErr, time.Now()); limit != nil {
				return limit
			}
			return &accounts.ErrAllExhausted{ProviderID: providerCfg.ID}
		}
		if err := b.applyRotationPick(ctx, providerCfg.ID, picked, active, rebuild); err != nil {
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

// usageFor reads accountID's last recorded usage snapshot, translating
// Sennit's own account id into the one the snapshot is filed under.
//
// The translation is the whole point. The usage store is keyed by the
// PROVIDER's account id - the chatgpt-account-id header the usage
// transport reads off the request it is holding (internal/oauth/codex) -
// while every id on this path is Sennit's own record id
// ("acc_<uuid>" against the provider's bare "<uuid>"). Looking the
// snapshot up under the record id found nothing, ever: threshold rotation
// never fired for Codex, and a spent window could not be recognized when
// a 429 arrived. all is the account list to translate through; an account
// that is not in it, or a provider whose accounts carry no id of their
// own, falls back to the id as given, which is right for a plan whose
// responses carry no account header at all (the store files those under
// the empty id, exactly as Account.AccountID is empty for them).
//
// Reports false when no lookup is wired or the account has no snapshot
// yet - both mean nothing is known about this account's windows.
func (b *runtimeBuilder) usageFor(accountID string, all []accounts.Account) (codex.Usage, bool) {
	if b.codexUsage == nil {
		return codex.Usage{}, false
	}
	for _, a := range all {
		if a.ID != accountID {
			continue
		}
		if usage, ok := b.codexUsage(a.AccountID); ok {
			return usage, true
		}
		break
	}
	// No record, or nothing filed under the provider's id for it: try the
	// id as given. A provider whose accounts carry no id of their own is
	// keyed by whatever the caller names them by, and this is also what
	// keeps a store written before the translation existed readable.
	usage, ok := b.codexUsage(accountID)
	if !ok {
		return codex.Usage{}, false
	}
	return usage, true
}

// freshenUsage overlays this process's own usage snapshots onto the stored
// account list, so Pick judges every candidate on what the provider said
// last rather than on whatever was last written to accounts.json.
//
// Without it a rotation walks straight into an account whose window is
// just as spent as the one it is leaving - which is what the reported case
// did: two accounts, both at 100%, and the turn spent its retry budget
// bouncing between them. An account with no snapshot is left exactly as
// stored: "not known to be spent" is what lets a freshly added account be
// used at all (see Rotator.Pick).
func (b *runtimeBuilder) freshenUsage(all []accounts.Account) []accounts.Account {
	if b.codexUsage == nil {
		return all
	}
	freshened := make([]accounts.Account, len(all))
	copy(freshened, all)
	for i, a := range freshened {
		if usage, ok := b.usageFor(a.ID, all); ok {
			freshened[i].Usage = usage.Snapshot()
		}
	}
	return freshened
}

// providerLimitFromUsage turns a 429 into a ProviderLimitError when the
// account's own usage snapshot explains it: a window the plan actually has
// is spent, and its reset still lies ahead. It reports nil otherwise, which
// leaves the caller on the ordinary rate-limit path - a passing burst, or a
// provider that quotes no windows, must keep retrying as it always did.
func providerLimitFromUsage(providerCfg config.ProviderConfig, usage codex.Usage, providerErr *fantasy.ProviderError, now time.Time) *ProviderLimitError {
	window, ok := spentWindow(usage, now)
	if !ok {
		return nil
	}
	return &ProviderLimitError{
		Provider:      providerCfg.ID,
		ProviderName:  providerCfg.Name,
		Plan:          usage.Plan,
		WindowMinutes: window.WindowMinutes,
		UsedPercent:   window.UsedPercent,
		ResetsAt:      window.ResetsAt,
		err:           providerErr,
	}
}

// providerLimitExhausted is providerLimitFromUsage for the case where every
// configured account is spent.
//
// It still requires the active account's own snapshot to show a spent
// window: that snapshot is the only evidence that this 429 is a
// subscription limit rather than a passing burst, and Pick reports "all
// exhausted" for its own cooldown bookkeeping as well, which says nothing
// about the provider's plan. Without that evidence the caller keeps the
// unchanged *accounts.ErrAllExhausted it always returned.
//
// Given the evidence, Pick's ResetsAt is the better time to quote: it is
// the earliest moment ANY account comes back, which is when the work can
// actually go on, and that is usually earlier than the window the turn's
// own account is waiting on.
func providerLimitExhausted(providerCfg config.ProviderConfig, usage codex.Usage, exhausted *accounts.ErrAllExhausted, providerErr *fantasy.ProviderError) *ProviderLimitError {
	limit := providerLimitFromUsage(providerCfg, usage, providerErr, time.Now())
	if limit == nil {
		return nil
	}
	limit.AllAccounts = true
	if !exhausted.ResetsAt.IsZero() {
		limit.ResetsAt = exhausted.ResetsAt
	}
	return limit
}

// rateLimitCooldown is how long MarkRateLimited should hold the account
// that just answered 429. A Retry-After header is the provider speaking
// about this very refusal and wins outright; with none, a spent
// subscription window is the next best thing, and it is a far better
// answer than the policy's flat default - a weekly window resets in days,
// and a rotator that tries the account again ten minutes later just
// collects another 429. Zero (let the policy decide) when neither is
// known.
func rateLimitCooldown(providerErr *fantasy.ProviderError, usage codex.Usage, now time.Time) time.Duration {
	if after := retryAfterFromHeaders(providerErr); after > 0 {
		return after
	}
	if window, ok := spentWindow(usage, now); ok {
		return window.ResetsAt.Add(resumeGrace).Sub(now)
	}
	return 0
}

// retryAfterFromHeaders extracts the Retry-After delay from a
// *fantasy.ProviderError's response headers, for MarkRateLimited. This
// deliberately duplicates the couple of lines third_party/fantasy/retry.go's
// unexported getRetryDelayInMs already does (retry-after-ms, then
// Retry-After as seconds or an HTTP date) rather than exporting that
// helper across the vendor boundary for one small caller, keeping the
// fork's surface area minimal.
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
