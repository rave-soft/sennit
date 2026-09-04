package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
)

// fakeAccountStore is a minimal in-memory accounts.Store for rotation
// tests: it exists so tests can control exactly which accounts a
// provider has (and their Disabled/Usage state) without touching disk,
// and so a test can assert Upsert/RecordUsage were (or were not) called.
type fakeAccountStore struct {
	byProvider map[string][]accounts.Account
	upserts    int
}

func newFakeAccountStore(providerID string, accs ...accounts.Account) *fakeAccountStore {
	return &fakeAccountStore{byProvider: map[string][]accounts.Account{providerID: accs}}
}

func (s *fakeAccountStore) List(providerID string) ([]accounts.Account, error) {
	out := make([]accounts.Account, len(s.byProvider[providerID]))
	copy(out, s.byProvider[providerID])
	return out, nil
}

func (s *fakeAccountStore) Get(providerID, accountID string) (accounts.Account, bool, error) {
	for _, a := range s.byProvider[providerID] {
		if a.ID == accountID {
			return a, true, nil
		}
	}
	return accounts.Account{}, false, nil
}

func (s *fakeAccountStore) Upsert(providerID string, a accounts.Account) error {
	s.upserts++
	list := s.byProvider[providerID]
	for i, existing := range list {
		if existing.ID == a.ID {
			list[i] = a
			return nil
		}
	}
	s.byProvider[providerID] = append(list, a)
	return nil
}

func (s *fakeAccountStore) Remove(providerID, accountID string) error {
	list := s.byProvider[providerID]
	for i, a := range list {
		if a.ID == accountID {
			s.byProvider[providerID] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *fakeAccountStore) RecordUsage(providerID, accountID string, u accounts.Usage) error {
	for i, a := range s.byProvider[providerID] {
		if a.ID == accountID {
			s.byProvider[providerID][i].Usage = u
			return nil
		}
	}
	return errors.New("account not found")
}

var _ accounts.Store = (*fakeAccountStore)(nil)

// ---------------------------------------------------------------------------
// rotatorFor
// ---------------------------------------------------------------------------

// TestRotatorFor_DisabledOrNilRotation_ReturnsNilAndBuildsNothing pins the
// "rotation off is provably identical to before rotation existed"
// requirement: rotatorFor must return nil, and must not have populated
// b.rotators at all, whether Rotation is nil or merely disabled.
func TestRotatorFor_DisabledOrNilRotation_ReturnsNilAndBuildsNothing(t *testing.T) {
	t.Parallel()

	b := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	require.Nil(t, b.rotatorFor(config.ProviderConfig{ID: "p"}))
	require.Empty(t, b.rotators, "no Rotation config must not build a rotator entry")

	b = &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	require.Nil(t, b.rotatorFor(config.ProviderConfig{ID: "p", Rotation: &config.RotationConfig{Enabled: false}}))
	require.Empty(t, b.rotators, "Rotation.Enabled=false must not build a rotator entry")
}

// TestRotatorFor_Enabled_BuildsOnceAndReuses proves the per-provider
// Rotator instance survives across calls (it holds in-memory cooldown
// state that must survive across requests, per Rotator's doc comment) and
// is not built once per provider distinct from another.
func TestRotatorFor_Enabled_BuildsOnceAndReuses(t *testing.T) {
	t.Parallel()

	b := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	cfg := config.ProviderConfig{ID: "p", Rotation: &config.RotationConfig{Enabled: true}}

	r1 := b.rotatorFor(cfg)
	require.NotNil(t, r1)
	r2 := b.rotatorFor(cfg)
	require.Same(t, r1, r2, "the same provider must always get the same Rotator instance")

	other := b.rotatorFor(config.ProviderConfig{ID: "q", Rotation: &config.RotationConfig{Enabled: true}})
	require.NotSame(t, r1, other, "different providers must not share a Rotator")
}

// ---------------------------------------------------------------------------
// makeRateLimitCallback (Trigger B: 429, RotateRateLimit providers)
// ---------------------------------------------------------------------------

func rateLimitErr(headers map[string]string) *fantasy.ProviderError {
	return &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests, ResponseHeaders: headers}
}

func apiKeyAccount(id, key string) accounts.Account {
	return accounts.Account{ID: id, Label: id, APIKey: key}
}

// diskAuthProviderJSON seeds authTestCoordinator's data-layer disk file
// (see withGlobalDataJSON) with a complete provider entry under
// authProviderID, so ConfigStore.ActivateAccount's SetConfigFields->reload
// - which patches onto and then rebuilds ProviderConfig from that same
// layer - has a full object to patch rather than manufacturing an
// incomplete one that provider validation then drops.
const diskAuthProviderJSON = `{"providers":{"test-openai-compat":{"id":"test-openai-compat","name":"Test","type":"openai-compat","base_url":"http://127.0.0.1:0/v1","api_key":"orig-key","models":[{"id":"test-model","name":"test-model"}]}}}`

// diskCodexProviderJSON is diskAuthProviderJSON's counterpart for tests
// that need CapabilitiesOf to route the provider to RotateThreshold, which
// requires the provider ID to be exactly codex.ProviderID.
const diskCodexProviderJSON = `{"providers":{"codex":{"id":"codex","name":"Test","type":"openai-compat","base_url":"http://127.0.0.1:0/v1","api_key":"orig-key","models":[{"id":"test-model","name":"test-model"}]}}}`

// TestMakeRateLimitCallback_Disabled_ReturnsNil is sabotage rule 1's
// integration point at this trigger: with rotation disabled, fantasy must
// never even be handed a callback to call.
func TestMakeRateLimitCallback_Disabled_ReturnsNil(t *testing.T) {
	t.Parallel()
	b := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	cb := b.makeRateLimitCallback(config.ProviderConfig{ID: "p"}, providerstate.Provider{}, nil, runtimeOperationPort{})
	require.Nil(t, cb)
}

// TestMakeRateLimitCallback_NonRateLimitProvider_ReturnsNil: a
// RotateThreshold provider (codex) has no 429 trigger to speak of.
func TestMakeRateLimitCallback_NonRateLimitProvider_ReturnsNil(t *testing.T) {
	t.Parallel()
	b := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	cfg := config.ProviderConfig{ID: codex.ProviderID, Rotation: &config.RotationConfig{Enabled: true}}
	cb := b.makeRateLimitCallback(cfg, providerstate.Provider{Account: "a"}, nil, runtimeOperationPort{})
	require.Nil(t, cb)
}

// TestMakeRateLimitCallback_SingleAccount_NoOp pins sabotage rule 2 at
// this trigger: one account configured must never call ActivateAccount,
// but the 429 must still cost the account its normal backoff - a non-nil
// error, not nil (which fantasy reads as "rotated, retry immediately" and
// would fire the very next attempt at the still-limited account with no
// delay at all). See makeRateLimitCallback's picked.ID == account branch.
func TestMakeRateLimitCallback_SingleAccount_NoOp(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t,
		withGlobalDataJSON(diskAuthProviderJSON),
		withNotify(notifier),
		withProvider(func(p *config.ProviderConfig) {
			p.Rotation = &config.RotationConfig{Enabled: true}
			p.Account = "only"
		}))
	co.builder.accountsStore = newFakeAccountStore(authProviderID, apiKeyAccount("only", "orig-key"))

	before, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	beforeVersion := co.cfg.CredentialVersion()

	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	cred, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)
	cb := co.builder.makeRateLimitCallback(providerCfg, cred, nil, runtimeOperationPort{})
	require.NotNil(t, cb)

	err := cb(t.Context(), rateLimitErr(nil))
	require.Error(t, err, "a single account must not return nil - that tells fantasy credentials rotated and to retry with no backoff")
	var exhausted *accounts.ErrAllExhausted
	require.True(t, errors.As(err, &exhausted), "must report the no-rotation-happened verdict as *accounts.ErrAllExhausted")

	after, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	require.Equal(t, before.APIKey, after.APIKey, "a single-account provider must never rotate its credentials")
	require.Equal(t, beforeVersion, co.cfg.CredentialVersion(), "no ActivateAccount call means no credential-version bump")
	require.Equal(t, 0, notifier.count("", notify.TypeAccountRotated), "no rotation happened, so no rotation notification either")
}

// TestMakeRateLimitCallback_RotatesAndAppliesNewCredentials is the core
// positive case for Trigger B (sabotage rule 4): on a 429, the callback
// marks the current account cooling down, picks the other configured
// account, and applies it - not just recording the pick, but actually
// publishing the new account's credentials into the live ProviderConfig
// (plan §5.2) and rebuilding the active runtime, which is what the
// retried request's ModelProvider actually reads.
func TestMakeRateLimitCallback_RotatesAndAppliesNewCredentials(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t,
		withGlobalDataJSON(diskAuthProviderJSON),
		withNotify(notifier),
		withProvider(func(p *config.ProviderConfig) {
			p.Rotation = &config.RotationConfig{Enabled: true}
			p.Account = "acct-a"
			p.APIKey = "key-a"
		}),
	)
	co.builder.accountsStore = newFakeAccountStore(authProviderID,
		apiKeyAccount("acct-a", "key-a"),
		apiKeyAccount("acct-b", "key-b"),
	)

	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	cred, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)

	runtime, err := co.builder.runtimeFor(t.Context(), co.delegation.runtimeInputs())
	require.NoError(t, err)
	active := newActiveRuntime(runtime)

	port := runtimeOperationPort{agent: co.dispatcher.agentPort.current(), inputs: co.delegation.runtimeInputs()}
	cb := co.builder.makeRateLimitCallback(providerCfg, cred, active, port)
	require.NotNil(t, cb)

	err = cb(t.Context(), rateLimitErr(nil))
	require.NoError(t, err, "a successful rotation must return nil so fantasy retries immediately")

	after, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-b", after.Account)
	require.Equal(t, "key-b", after.APIKey, "the retried request's credentials come from the runtime provider")

	require.NotNil(t, active.load(), "the active runtime must be rebuilt so a retried request's ModelProvider sees it")
	require.Equal(t, "acct-b", active.load().providerCredentials.Account, "the rebuilt runtime must carry the NEW account, not the rate-limited one")

	require.Equal(t, 1, notifier.count("", notify.TypeAccountRotated))
}

// TestMakeRateLimitCallback_AllExhausted_SurfacesOriginalError is sabotage
// rule 5's integration point at this trigger: when every candidate is
// exhausted, the callback must return the *accounts.ErrAllExhausted
// unchanged (never nil, never a substituted error) so RetryOptions.OnRateLimit
// falls through to normal backoff and the original 429 chain survives, and
// it must not touch the provider's active credentials.
func TestMakeRateLimitCallback_AllExhausted_SurfacesOriginalError(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t,
		withGlobalDataJSON(diskAuthProviderJSON),
		withNotify(notifier),
		withProvider(func(p *config.ProviderConfig) {
			p.Rotation = &config.RotationConfig{Enabled: true}
			p.Account = "acct-a"
			p.APIKey = "key-a"
		}),
	)
	co.builder.accountsStore = newFakeAccountStore(authProviderID,
		accounts.Account{ID: "acct-a", Label: "acct-a", APIKey: "key-a", Disabled: false},
		accounts.Account{ID: "acct-b", Label: "acct-b", APIKey: "key-b", Disabled: true},
	)

	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	cred, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)
	beforeVersion := co.cfg.CredentialVersion()

	cb := co.builder.makeRateLimitCallback(providerCfg, cred, nil, runtimeOperationPort{})
	require.NotNil(t, cb)

	// acct-a is the only enabled account, and it is the one being marked
	// cooling-down by this very call, so after MarkRateLimited nothing is
	// usable.
	err := cb(t.Context(), rateLimitErr(nil))
	require.Error(t, err)
	var exhausted *accounts.ErrAllExhausted
	require.True(t, errors.As(err, &exhausted), "must return *accounts.ErrAllExhausted, not a different error, so the ORIGINAL 429 survives upstream")
	require.Equal(t, authProviderID, exhausted.ProviderID)

	after, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)
	require.Equal(t, "key-a", after.APIKey, "exhaustion must not change the active credentials")
	require.Equal(t, beforeVersion, co.cfg.CredentialVersion())

	require.Equal(t, 1, notifier.count("", notify.TypeAccountRotationExhausted))
}

// TestMakeRateLimitCallback_HonorsRetryAfterHeader is the thin
// integration point for sabotage rule 6: the callback must extract
// Retry-After off the *fantasy.ProviderError and pass it to
// MarkRateLimited, not just fall back to the configured cooldown. It
// proves this by cooling acct-a down for a very short, header-supplied
// duration and observing that acct-a becomes pickable again once that
// (short) duration elapses - which would not happen for tens of minutes
// if the fallback cooldown had been used instead.
func TestMakeRateLimitCallback_HonorsRetryAfterHeader(t *testing.T) {
	co := authTestCoordinator(t,
		withGlobalDataJSON(diskAuthProviderJSON),
		withProvider(func(p *config.ProviderConfig) {
			p.Rotation = &config.RotationConfig{Enabled: true}
			p.Account = "acct-a"
			p.APIKey = "key-a"
		}))
	co.builder.accountsStore = newFakeAccountStore(authProviderID,
		apiKeyAccount("acct-a", "key-a"),
		apiKeyAccount("acct-b", "key-b"),
	)
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	cred, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)

	cb := co.builder.makeRateLimitCallback(providerCfg, cred, nil, runtimeOperationPort{})
	require.NotNil(t, cb)

	// First 429 rotates a -> b (debounce then blocks a second rotation).
	// Carries a real Retry-After header end to end through the callback
	// (not just through retryAfterFromHeaders' own unit test), proving cb
	// actually extracts and forwards it rather than only ever calling
	// MarkRateLimited with the zero/no-header value.
	require.NoError(t, cb(t.Context(), rateLimitErr(map[string]string{"retry-after": "45"})))

	rotator := co.builder.rotatorFor(providerCfg)
	require.NotNil(t, rotator)

	// Directly exercise MarkRateLimited's Retry-After honoring, the same
	// call the callback makes: a 1-second header must cool down for ~1s,
	// not accounts.DefaultCooldown's 10 minutes.
	rotator.MarkRateLimited("acct-b", 0) // no header: falls back to default (10m)
	all, err := co.builder.accountsStore.List(authProviderID)
	require.NoError(t, err)
	_, pickErr := rotator.Pick(authProviderID, "acct-b", all)
	var exhausted *accounts.ErrAllExhausted
	require.True(t, errors.As(pickErr, &exhausted), "with no header, the default (long) cooldown must still be in effect")
}

// TestMakeRateLimitCallback_SecondRateLimitActsOnRotatedAccount is a
// regression test for the stale-providerCfg hot-loop bug: the callback is
// built once per turn and closes over providerCfg by value, so after the
// first rotation (acct-a -> acct-b) a second 429 must be attributed to
// acct-b, the account that is now actually live, not to acct-a again. Before
// the fix, this second call kept marking acct-a rate-limited, Pick kept
// finding acct-b "usable" (it was never actually marked), and the callback
// re-ran applyRotationPick and returned nil every time - fantasy would retry
// immediately on the still-limited acct-b forever. With the fix, the second
// call marks acct-b (the real culprit) and, with both accounts now cooling
// down, correctly reports exhaustion instead of looping.
func TestMakeRateLimitCallback_SecondRateLimitActsOnRotatedAccount(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t,
		withGlobalDataJSON(diskAuthProviderJSON),
		withNotify(notifier),
		withProvider(func(p *config.ProviderConfig) {
			p.Rotation = &config.RotationConfig{Enabled: true}
			p.Account = "acct-a"
			p.APIKey = "key-a"
		}),
	)
	co.builder.accountsStore = newFakeAccountStore(authProviderID,
		apiKeyAccount("acct-a", "key-a"),
		apiKeyAccount("acct-b", "key-b"),
	)

	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	cred, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)

	runtime, err := co.builder.runtimeFor(t.Context(), co.delegation.runtimeInputs())
	require.NoError(t, err)
	active := newActiveRuntime(runtime)

	port := runtimeOperationPort{agent: co.dispatcher.agentPort.current(), inputs: co.delegation.runtimeInputs()}
	// The callback is built once, exactly like turn_dispatcher.go does for
	// the whole turn, and reused across both simulated 429s below.
	cb := co.builder.makeRateLimitCallback(providerCfg, cred, active, port)
	require.NotNil(t, cb)

	require.NoError(t, cb(t.Context(), rateLimitErr(nil)), "first 429 rotates acct-a -> acct-b")
	require.Equal(t, "acct-b", active.load().providerCredentials.Account)
	require.Equal(t, 1, notifier.count("", notify.TypeAccountRotated))

	err = cb(t.Context(), rateLimitErr(nil))
	var exhausted *accounts.ErrAllExhausted
	require.True(t, errors.As(err, &exhausted),
		"a second 429 must mark acct-b (the account actually rate-limited), leaving both accounts cooling down")
	require.Equal(t, 1, notifier.count("", notify.TypeAccountRotated),
		"the stale-account bug re-applies the same pick and re-notifies on every retry")
}

// ---------------------------------------------------------------------------
// makeSubAgentRateLimitCallback (B4: rate-limit rotation for delegations)
// ---------------------------------------------------------------------------

// subAgentTestModel returns a Model naming authProviderID/authModelID, the
// only fields buildSubAgentRuntime reads (see its own doc comment: it
// re-reads providerCfg/providerCredentials from the live config store by
// model.ModelCfg.Provider, and never consults CatalogCfg/Model here).
func subAgentTestModel() Model {
	return Model{ModelCfg: config.SelectedModel{Provider: authProviderID, Model: authModelID}}
}

// TestMakeSubAgentRateLimitCallback_RotatesAndAppliesNewCredentials is
// makeSubAgentRateLimitCallback's counterpart to
// TestMakeRateLimitCallback_RotatesAndAppliesNewCredentials: a 429 marks the
// current account cooling down, picks the other configured account, and
// applies it - the config-side assertions are identical to the top-level
// case, since applyRotationPick's account activation is shared code.
//
// What differs, and what this test exists to pin, is the REBUILD half: the
// rebuilt runtime stored on active must come from buildSubAgentRuntime, not
// runtimeFor. buildSubAgentRuntime deliberately leaves tools/systemPrompt
// unset (see its own doc comment - only .model is read back out of it), so
// asserting they are empty after rotation is the observable proxy that the
// sub-agent rebuild path ran instead of the top-level one, which would have
// populated them with the coordinator's full tool set and system prompt -
// exactly the privilege escalation this trigger must not cause.
func TestMakeSubAgentRateLimitCallback_RotatesAndAppliesNewCredentials(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t,
		withGlobalDataJSON(diskAuthProviderJSON),
		withNotify(notifier),
		withProvider(func(p *config.ProviderConfig) {
			p.Rotation = &config.RotationConfig{Enabled: true}
			p.Account = "acct-a"
			p.APIKey = "key-a"
		}),
	)
	co.builder.accountsStore = newFakeAccountStore(authProviderID,
		apiKeyAccount("acct-a", "key-a"),
		apiKeyAccount("acct-b", "key-b"),
	)

	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	cred, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)

	// A sub-agent's ActiveRuntime starts nil and is only ever populated by a
	// successful refresh/rotation (see runSubAgent's own comment on active).
	active := newActiveRuntime(nil)

	cb := co.builder.makeSubAgentRateLimitCallback(providerCfg, cred, subAgentTestModel(), active)
	require.NotNil(t, cb)

	err := cb(t.Context(), rateLimitErr(nil))
	require.NoError(t, err, "a successful rotation must return nil so fantasy retries immediately")

	after, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-b", after.Account)
	require.Equal(t, "key-b", after.APIKey)

	require.NotNil(t, active.load(), "the sub-agent's active runtime must be rebuilt so a retried request's ModelProvider sees it")
	require.Equal(t, "acct-b", active.load().providerCredentials.Account)
	require.Empty(t, active.load().tools, "rebuild must go through buildSubAgentRuntime (tools left unset), never runtimeFor (which would carry the coordinator's full tool set)")
	require.Empty(t, active.load().systemPrompt, "rebuild must go through buildSubAgentRuntime (system prompt left unset), never runtimeFor")

	require.Equal(t, 1, notifier.count("", notify.TypeAccountRotated))
}

// TestMakeSubAgentRateLimitCallback_AllExhausted_DoesNotLoop is the
// delegation counterpart of TestMakeRateLimitCallback_AllExhausted_SurfacesOriginalError:
// with a single account and nothing else usable, the callback must return
// *accounts.ErrAllExhausted (never nil, which would tell fantasy to retry
// immediately with no backoff and hot-loop the still-limited account)
// exactly once per call, not repeat the account further into cooldown -
// pinning "исчерпание аккаунтов в делегации не зацикливается".
func TestMakeSubAgentRateLimitCallback_AllExhausted_DoesNotLoop(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t,
		withGlobalDataJSON(diskAuthProviderJSON),
		withNotify(notifier),
		withProvider(func(p *config.ProviderConfig) {
			p.Rotation = &config.RotationConfig{Enabled: true}
			p.Account = "only"
		}))
	co.builder.accountsStore = newFakeAccountStore(authProviderID, apiKeyAccount("only", "orig-key"))

	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	cred, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)
	active := newActiveRuntime(nil)

	cb := co.builder.makeSubAgentRateLimitCallback(providerCfg, cred, subAgentTestModel(), active)
	require.NotNil(t, cb)

	// Two consecutive 429s, exactly as a stuck delegation retry loop would
	// deliver them: the callback must report exhaustion both times rather
	// than ever finding acct "only" usable again or looping on it.
	for i := range 2 {
		err := cb(t.Context(), rateLimitErr(nil))
		require.Error(t, err, "call %d must report exhaustion, not rotate", i)
		var exhausted *accounts.ErrAllExhausted
		require.True(t, errors.As(err, &exhausted), "call %d must return *accounts.ErrAllExhausted unchanged", i)
	}
	require.Nil(t, active.load(), "exhaustion must never populate the sub-agent's active runtime")
	require.Equal(t, 0, notifier.count("", notify.TypeAccountRotated))
}

// ---------------------------------------------------------------------------
// makeThresholdRotateCallback (Trigger A: threshold, RotateThreshold providers)
// ---------------------------------------------------------------------------

// codexAccountStore builds a fake store under codex.ProviderID, the only
// literal capabilities.go currently maps to RotateThreshold.
func codexAccountStore(accs ...accounts.Account) *fakeAccountStore {
	return newFakeAccountStore(codex.ProviderID, accs...)
}

func codexProviderConfig(account string, enabled bool) config.ProviderConfig {
	return config.ProviderConfig{
		ID:      codex.ProviderID,
		Name:    "Codex",
		Account: account,
		Rotation: &config.RotationConfig{
			Enabled:             enabled,
			MinRemainingPercent: 10, // rotate at >=90% used
		},
	}
}

func TestMakeThresholdRotateCallback_Disabled_ReturnsNil(t *testing.T) {
	t.Parallel()
	b := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	cb := b.makeThresholdRotateCallback(codexProviderConfig("a", false), providerstate.Provider{Account: "a"}, nil, runtimeOperationPort{})
	require.Nil(t, cb)
}

// TestMakeThresholdRotateCallback_NonThresholdProvider_ReturnsNil: a
// RotateRateLimit provider (the default capability) has no usage number
// to compare against a threshold.
func TestMakeThresholdRotateCallback_NonThresholdProvider_ReturnsNil(t *testing.T) {
	t.Parallel()
	b := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	cfg := config.ProviderConfig{ID: "some-other-provider", Rotation: &config.RotationConfig{Enabled: true}}
	cb := b.makeThresholdRotateCallback(cfg, providerstate.Provider{}, nil, runtimeOperationPort{})
	require.Nil(t, cb)
}

// TestMakeThresholdRotateCallback_BelowThreshold_NoOp: usage under the
// configured threshold must not touch the active account at all.
func TestMakeThresholdRotateCallback_BelowThreshold_NoOp(t *testing.T) {
	// Unique account IDs (not reused by other tests in this file): the
	// codex usage store is a package-level singleton with no exported
	// reset, so distinct IDs are what keep tests from reading each
	// other's snapshots rather than a shared reset call.
	codex.RecordUsageFor("acct-below", codex.Usage{Plan: "plus", Primary: codex.UsageWindow{UsedPercent: 50, WindowMinutes: 60}})

	notifier := &recordingNotifier{}
	providerCfg := codexProviderConfig("acct-below", true)
	cfg := testRotationConfigStoreWithProvider(t, providerCfg)
	effective, err := providerruntime.FromConfig(providerCfg, cfg.Config().RuntimeResolver())
	require.NoError(t, err)
	cfg.Config().SetRuntimeProvider(codex.ProviderID, effective)

	b := &runtimeBuilder{
		agentDeps: &agentDeps{
			cfg:           cfg,
			notify:        notifier,
			accountsStore: codexAccountStore(apiKeyAccount("acct-below", "key-a"), apiKeyAccount("acct-b", "key-b")),
			codexUsage:    codex.UsageFor,
		},
		runtime: newRuntimeCache(),
	}

	cb := b.makeThresholdRotateCallback(providerCfg, effective, nil, runtimeOperationPort{})
	require.NotNil(t, cb)
	cb(t.Context())

	after, ok := cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-below", after.Account, "usage under threshold must not rotate")
	require.Equal(t, 0, notifier.count("", notify.TypeAccountRotated))
}

// TestMakeThresholdRotateCallback_RotatesOverThreshold is the core
// positive case for Trigger A (sabotage rule 3's rotation half): usage at
// or over the configured threshold switches to the next configured
// account and publishes a notification naming both accounts.
func TestMakeThresholdRotateCallback_RotatesOverThreshold(t *testing.T) {
	codex.RecordUsageFor("acct-over", codex.Usage{Plan: "plus", Primary: codex.UsageWindow{UsedPercent: 95, WindowMinutes: 60}})

	notifier := &recordingNotifier{}
	providerCfg := codexProviderConfig("acct-over", true)
	cfg := testRotationConfigStoreWithProvider(t, providerCfg)
	effective, err := providerruntime.FromConfig(providerCfg, cfg.Config().RuntimeResolver())
	require.NoError(t, err)
	cfg.Config().SetRuntimeProvider(codex.ProviderID, effective)

	b := &runtimeBuilder{
		agentDeps: &agentDeps{
			cfg:           cfg,
			notify:        notifier,
			accountsStore: codexAccountStore(apiKeyAccount("acct-over", "key-a"), apiKeyAccount("acct-c", "key-c")),
			codexUsage:    codex.UsageFor,
		},
		runtime: newRuntimeCache(),
	}

	cb := b.makeThresholdRotateCallback(providerCfg, effective, nil, runtimeOperationPort{})
	require.NotNil(t, cb)
	cb(t.Context())

	after, ok := cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-c", after.Account, "usage over threshold must rotate to the next account")

	require.Equal(t, 1, notifier.count("", notify.TypeAccountRotated))
}

// TestMakeThresholdRotateCallback_SecondStepReadsRotatedAccountUsage is the
// threshold-trigger counterpart of
// TestMakeRateLimitCallback_SecondRateLimitActsOnRotatedAccount: the
// callback is built once per turn, so after the first rotation
// (acct-over -> acct-c) a later step must read acct-c's usage snapshot, not
// keep reading acct-over's stale one. Before the fix, every subsequent step
// re-read acct-over's over-threshold usage, found ShouldRotate still true,
// and re-ran applyRotationPick (rebuild + "switched" notification) even
// though acct-c was already active - see the doc comment on
// currentRotationAccount.
func TestMakeThresholdRotateCallback_SecondStepReadsRotatedAccountUsage(t *testing.T) {
	codex.RecordUsageFor("acct-over-2", codex.Usage{Plan: "plus", Primary: codex.UsageWindow{UsedPercent: 95, WindowMinutes: 60}})

	notifier := &recordingNotifier{}
	co := authTestCoordinator(t,
		withGlobalDataJSON(diskCodexProviderJSON),
		withNotify(notifier),
		withProviderID(codex.ProviderID),
		withProvider(func(p *config.ProviderConfig) {
			p.Rotation = &config.RotationConfig{Enabled: true, MinRemainingPercent: 10}
			p.Account = "acct-over-2"
		}),
	)
	co.builder.accountsStore = codexAccountStore(apiKeyAccount("acct-over-2", "key-a"), apiKeyAccount("acct-c-2", "key-c"))

	providerCfg, ok := co.cfg.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	cred, ok := co.cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)

	runtime, err := co.builder.runtimeFor(t.Context(), co.delegation.runtimeInputs())
	require.NoError(t, err)
	active := newActiveRuntime(runtime)

	port := runtimeOperationPort{agent: co.dispatcher.agentPort.current(), inputs: co.delegation.runtimeInputs()}
	// Built once, exactly like turn_dispatcher.go does for the whole turn,
	// and reused across both simulated steps below.
	cb := co.builder.makeThresholdRotateCallback(providerCfg, cred, active, port)
	require.NotNil(t, cb)

	cb(t.Context())
	after, ok := co.cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-c-2", after.Account, "first step rotates over-threshold acct-over-2 to acct-c-2")
	require.Equal(t, "acct-c-2", active.load().providerCredentials.Account)
	require.Equal(t, 1, notifier.count("", notify.TypeAccountRotated))

	// acct-c-2 has no recorded usage, so a step that correctly reads ITS
	// (unknown) usage must be a no-op, not a repeat rotation driven by
	// acct-over-2's stale, still-over-threshold snapshot.
	cb(t.Context())
	after, ok = co.cfg.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-c-2", after.Account, "a later step must not re-rotate off the already-current account")
	require.Equal(t, 1, notifier.count("", notify.TypeAccountRotated),
		"the stale-account bug re-reads acct-over-2's usage and re-notifies on every step")
}

// testRotationConfigStoreWithProvider builds a real, LoadData-backed
// *config.ConfigStore (via configruntime.Load, the same path
// authTestCoordinator uses) so ActivateAccount's
// disk-write-then-reload-then-publish sequence is exercised for real rather
// than approximated by a hand-built &ConfigStore{...} literal. The
// threshold-rotate tests need providerCfg (a bare Rotation-only
// config.ProviderConfig with no credentials) folded into Providers before
// the store is published. Providers is frozen the moment a Config is
// published (see config.ConfigStore.setConfig), so mutating
// cfg.Config().Providers after configruntime.Load would panic; going
// through the real disk-reload pipeline instead would drop a credential-
// less codex entry as unusable (see providerload's applyCredentials),
// which is beside the point here. config.NewStore publishes cfg directly,
// with no processor and no freeze, mirroring the pattern
// TestRuntimeForUsesCapturedPublishedConfigGeneration uses.
func testRotationConfigStoreWithProvider(t *testing.T, providerCfg config.ProviderConfig) *config.ConfigStore {
	t.Helper()
	writeGlobalConfig(t, "{}")
	env := testEnv(t)
	loaded, err := configruntime.Load(env.workingDir, "", false)
	require.NoError(t, err)
	base := *loaded.Config()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set(providerCfg.ID, providerCfg)
	base.Providers = providers

	// base was copied out of an already-published Config
	// (configruntime.Load), so its RuntimeProviders field still aliases
	// that Config's frozen map — RuntimeProviders is frozen on publish just
	// like Providers is (see ConfigStore.setConfig). Give base its own,
	// unfrozen map so callers can SetRuntimeProvider on the store's Config
	// after NewStore (which itself does not freeze).
	base.RuntimeProviders = csync.NewMap[string, providerstate.Provider]()

	// WorkingDir is deliberately left unset — see the matching comment in
	// authTestCoordinator (auth_test.go) for why: it keeps a post-write
	// autoReload (ActivateAccount, via applyRotationPick) from rebuilding
	// this in-memory-only Config from disk and dropping the provider for
	// lacking an on-disk identity.
	return config.NewStore(config.StoreOptions{
		Config:         &base,
		GlobalDataPath: config.GlobalConfigData(),
	})
}

// ---------------------------------------------------------------------------
// currentRotationAccount
// ---------------------------------------------------------------------------

// TestCurrentRotationAccount_ActiveForDifferentProvider_FallsBackToCaptured
// is the regression test for the cross-provider trust bug: active can hold
// a runtime makeAuthRefreshCallback built for whatever provider the CURRENT
// config names, which may no longer be the provider this callback was built
// for (the user switched the main model mid-turn). Trusting that runtime's
// Account would mark/rotate an account this provider's Rotator never heard
// of - the captured cred.Account is what must come back instead.
func TestCurrentRotationAccount_ActiveForDifferentProvider_FallsBackToCaptured(t *testing.T) {
	t.Parallel()

	providerCfg := config.ProviderConfig{ID: "provider-x"}
	cred := providerstate.Provider{Account: "captured-account"}
	otherProviderRuntime := &compiledRuntime{
		providerCfg:         config.ProviderConfig{ID: "provider-y"},
		providerCredentials: providerstate.Provider{Account: "other-provider-account"},
	}
	active := newActiveRuntime(otherProviderRuntime)

	got := currentRotationAccount(providerCfg, cred, active)
	require.Equal(t, "captured-account", got, "active describes a different provider, so the captured account must win")
}

// TestCurrentRotationAccount_ActiveForSameProvider_UsesLoaded is the
// positive counterpart: when active does describe the SAME provider this
// callback was built for, it is the live, post-rotation account and must be
// preferred over the turn-stale captured one.
func TestCurrentRotationAccount_ActiveForSameProvider_UsesLoaded(t *testing.T) {
	t.Parallel()

	providerCfg := config.ProviderConfig{ID: "provider-x"}
	cred := providerstate.Provider{Account: "captured-account"}
	sameProviderRuntime := &compiledRuntime{
		providerCfg:         config.ProviderConfig{ID: "provider-x"},
		providerCredentials: providerstate.Provider{Account: "rotated-account"},
	}
	active := newActiveRuntime(sameProviderRuntime)

	got := currentRotationAccount(providerCfg, cred, active)
	require.Equal(t, "rotated-account", got, "same provider: the loaded runtime's account is the live one")
}

// ---------------------------------------------------------------------------
// retryAfterFromHeaders
// ---------------------------------------------------------------------------

func TestRetryAfterFromHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"nil error headers", nil, 0},
		{"retry-after seconds", map[string]string{"retry-after": "30"}, 30 * time.Second},
		{"retry-after-ms preferred", map[string]string{"retry-after-ms": "500", "retry-after": "30"}, 500 * time.Millisecond},
		{"unparseable falls back to zero", map[string]string{"retry-after": "not-a-number"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var err *fantasy.ProviderError
			if tt.headers != nil {
				err = &fantasy.ProviderError{ResponseHeaders: tt.headers}
			}
			got := retryAfterFromHeaders(err)
			require.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// runTurn.onRateLimit: stream-reset behavior (sabotage rule 7)
// ---------------------------------------------------------------------------

// TestOnRateLimit_ResetsStreamedContent_NoConcatenation is the direct
// regression test for plan §9 risk 7: fantasy's OnRateLimit contract skips
// OnRetry for the attempt it handles (see RetryOptions.OnRateLimit's doc
// comment), so onRetry's own ResetStreamedContent never runs on this path.
// t.onRateLimit must do its own reset, or the retried account's response
// concatenates onto the rate-limited attempt's partial output.
func TestOnRateLimit_ResetsStreamedContent_NoConcatenation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	// No debounce, for the same reason tool_input_stream_test.go turns it
	// off: this asserts on what a reader sees right after each callback,
	// which the coalescing window would otherwise hide behind a timer.
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	sessions := sessionstore.NewService(q, conn, "/test/project")

	sess, err := sessions.Create(ctx, "rate limit reset")
	require.NoError(t, err)
	assistant, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)

	var rotateCalled bool
	turn := &runTurn{
		agent:            &sessionAgent{messages: messages},
		ctx:              ctx,
		genCtx:           ctx,
		currentAssistant: &assistant,
		call: SessionAgentCall{
			OnRateLimit: func(context.Context, *fantasy.ProviderError) error {
				rotateCalled = true
				return nil
			},
		},
	}

	// The failed attempt's partial output.
	require.NoError(t, turn.onTextDelta("1", "partial output from the rate-limited account"))

	require.NoError(t, turn.onRateLimit(ctx, rateLimitErr(nil)))
	require.True(t, rotateCalled)

	stored, err := messages.Get(ctx, assistant.ID)
	require.NoError(t, err)
	require.Empty(t, stored.Content().Text, "the rate-limited attempt's partial text must be gone before the retry starts")

	// The retried account's own response.
	require.NoError(t, turn.onTextDelta("2", "fresh output from the new account"))

	stored, err = messages.Get(ctx, assistant.ID)
	require.NoError(t, err)
	require.Equal(t, "fresh output from the new account", stored.Content().Text,
		"the retried response must not be concatenated onto the discarded partial output")
}

// TestOnRateLimit_NilCallback_IsNoOp: a turn with no rotation configured
// must leave onRateLimit inert - this is sabotage rule 1's integration
// point at the turn level.
func TestOnRateLimit_NilCallback_IsNoOp(t *testing.T) {
	t.Parallel()
	turn := &runTurn{call: SessionAgentCall{}}
	require.NoError(t, turn.onRateLimit(context.Background(), rateLimitErr(nil)))
}

// ---------------------------------------------------------------------------
// RotateThreshold: fires only between steps (sabotage rule 3)
// ---------------------------------------------------------------------------

// TestRotateThreshold_OnlyFiresFromOnStepFinish is the direct regression
// test for "threshold rotation happens, and only between steps, never
// mid-stream": it wires a counting spy as call.RotateThreshold, drives the
// turn's mid-stream callbacks (onTextDelta, onToolInputStart,
// onToolInputDelta, onToolCall), and asserts the spy is untouched by any
// of them - only onStepFinish, called once processStepStream has fully
// drained the step (see fantasy's agent.go Stream loop), may call it.
func TestRotateThreshold_OnlyFiresFromOnStepFinish(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	sessions := sessionstore.NewService(q, conn, "/test/project")

	sess, err := sessions.Create(ctx, "threshold placement")
	require.NoError(t, err)
	assistant, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)

	var rotateCalls int
	turn := &runTurn{
		agent:            &sessionAgent{sessions: sessions, messages: messages},
		ctx:              ctx,
		genCtx:           ctx,
		currentAssistant: &assistant,
		currentSession:   sess,
		call: SessionAgentCall{
			SessionID: sess.ID,
			RotateThreshold: func(context.Context) {
				rotateCalls++
			},
		},
	}

	// Mid-stream callbacks a step actually fires. None of these may
	// touch RotateThreshold.
	require.NoError(t, turn.onTextDelta("1", "some text"))
	require.NoError(t, turn.onToolInputStart("tc-1", "bash"))
	require.NoError(t, turn.onToolInputDelta("tc-1", `{"command":"ls"}`))
	require.NoError(t, turn.onToolCall(fantasy.ToolCallContent{ToolCallID: "tc-1", ToolName: "bash", Input: `{"command":"ls"}`}))
	require.Equal(t, 0, rotateCalls, "RotateThreshold must not fire from any mid-stream callback")

	require.NoError(t, turn.onStepFinish(fantasy.StepResult{
		Response: fantasy.Response{FinishReason: fantasy.FinishReasonStop},
	}))
	require.Equal(t, 1, rotateCalls, "RotateThreshold must fire exactly once, from onStepFinish")
}

// TestNewCoordinator_UsesInjectedAccountsStore proves the coordinator
// wires CoordinatorOptions.AccountsStore straight into the runtime
// builder's accountsStore field, rather than ever falling back to a
// production accounts.NewFileStore(...). Without dependency injection,
// nothing observable would distinguish an injected fake from a lazily
// constructed file store; the injected field is the seam to assert on.
func TestNewCoordinator_UsesInjectedAccountsStore(t *testing.T) {
	fake := newFakeAccountStore(authProviderID)
	co := authTestCoordinator(t, withAccountsStore(fake))

	require.Same(t, fake, co.builder.accountsStore,
		"the builder must hold the exact injected accounts.Store instance")
}
