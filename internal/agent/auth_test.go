package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/credentials"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// authProviderID and authModelID name the single offline provider/model
// authTestCoordinator wires up.
const (
	authProviderID = "test-openai-compat"
	authModelID    = "test-model"
)

// authCoordSettings collects authTestCoordinator's optional dependencies.
type authCoordSettings struct {
	notify        pubsub.Publisher[notify.Notification]
	exchangeToken func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error)
	configureProv func(*config.ProviderConfig)
}

type authCoordOpt func(*authCoordSettings)

// withNotify installs a notification publisher on the coordinator, e.g. a
// recordingNotifier, so a test can assert on published notify.Notification
// events.
func withNotify(n pubsub.Publisher[notify.Notification]) authCoordOpt {
	return func(s *authCoordSettings) { s.notify = n }
}

// withExchangeToken builds the coordinator's credentials.Manager with the
// given OAuth token exchange override (see credentials.WithExchangeToken),
// so a refresh test can drive a real Manager.RefreshOAuthToken call
// without making a network request.
func withExchangeToken(exchange func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error)) authCoordOpt {
	return func(s *authCoordSettings) { s.exchangeToken = exchange }
}

// withProvider mutates the single provider config authTestCoordinator
// installs before it is set on the config store. Use it to set fields
// like OAuthToken, AWSAuthRefresh, or APIKeyTemplate that a
// credential-refresh test needs to drive retryAfterUnauthorized's
// branches.
func withProvider(configure func(*config.ProviderConfig)) authCoordOpt {
	return func(s *authCoordSettings) { s.configureProv = configure }
}

// authTestCoordinator builds a fully production-wired *coordinator (through
// the real NewCoordinator constructor, exactly like production wiring)
// pointed at a hermetic, empty global config, so the credential-refresh
// code under test can call through to the real UpdateModels/runtimeFor
// path with no network I/O. The provider is openai-compatible and never
// dialed: LanguageModel construction is lazy, so an unroutable base URL is
// fine (same approach as coordinator_custom_model_test.go).
//
// Every test file in this package is `package agent`, to reach unexported
// methods, so none of them can share a helper that lives in a package
// importing this one -- that is an import cycle. The construction is
// therefore inline here rather than in a test-support package.
//
// Not safe for a t.Parallel() caller: writeGlobalConfig uses t.Setenv.
func authTestCoordinator(t *testing.T, opts ...authCoordOpt) *coordinator {
	t.Helper()
	var s authCoordSettings
	for _, apply := range opts {
		apply(&s)
	}

	writeGlobalConfig(t, "{}")
	env := testEnv(t)

	cfg, err := config.Load(env.workingDir, "", false)
	require.NoError(t, err)

	providerCfg := config.ProviderConfig{
		ID:      authProviderID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: authModelID, DefaultMaxTokens: 4096}},
	}
	if s.configureProv != nil {
		s.configureProv(&providerCfg)
	}
	cfg.Config().Providers.Set(authProviderID, providerCfg)
	cfg.OverridePreferredModel(config.SelectedModel{Provider: authProviderID, Model: authModelID})
	cfg.SetupAgents()

	// Keep buildTools light: no sub-agent or agentic-fetch construction.
	coderCfg := cfg.Config().Agents[config.AgentCoder]
	coderCfg.AllowedTools = nil
	cfg.Config().Agents[config.AgentCoder] = coderCfg

	var credOpts []credentials.Option
	if s.exchangeToken != nil {
		credOpts = append(credOpts, credentials.WithExchangeToken(s.exchangeToken))
	}
	creds := credentials.New(cfg, credOpts...)

	co, err := NewCoordinator(t.Context(), CoordinatorOptions{
		Config:           cfg,
		Credentials:      creds,
		Sessions:         env.sessions,
		Messages:         env.messages,
		Permissions:      env.permissions,
		BackgroundShells: shell.NewBackgroundShellManager(),
		Notify:           s.notify,
	})
	require.NoError(t, err)
	c, ok := co.(*coordinator)
	require.True(t, ok, "NewCoordinator must return a *coordinator")
	return c
}

// fakeExchange returns a token-exchange stub (see withExchangeToken) that
// always answers with result/err, regardless of provider or refresh
// token, so a test can drive a real credentials.Manager.RefreshOAuthToken
// call without a network request.
func fakeExchange(result *oauth.Token, err error) func(context.Context, string, string) (*oauth.Token, error) {
	return func(context.Context, string, string) (*oauth.Token, error) {
		return result, err
	}
}

// freshToken returns a valid, non-expired OAuth token.
func freshToken(accessToken string) *oauth.Token {
	tok := &oauth.Token{AccessToken: accessToken, RefreshToken: "refresh-" + accessToken, ExpiresIn: 3600}
	tok.SetExpiresAt()
	return tok
}

// expiredToken returns an OAuth token that IsExpired reports true for.
func expiredToken() *oauth.Token {
	const accessToken = "old-access"
	return &oauth.Token{
		AccessToken:  accessToken,
		RefreshToken: "refresh-" + accessToken,
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}
}

// revokedExchangeErr is a TokenExchangeError whose IsRefreshTokenRevoked
// reports true.
var revokedExchangeErr = &oauth.TokenExchangeError{StatusCode: 400, Body: "invalid_grant"}

// ---------------------------------------------------------------------------
// refreshTokenIfExpired
// ---------------------------------------------------------------------------

func TestRefreshTokenIfExpired_NilToken_NoOp(t *testing.T) {
	t.Parallel()
	c := &coordinator{}
	err := c.refreshTokenIfExpired(t.Context(), config.ProviderConfig{ID: "p"})
	require.NoError(t, err)
}

func TestRefreshTokenIfExpired_NotExpired_NoOp(t *testing.T) {
	t.Parallel()
	c := &coordinator{}
	err := c.refreshTokenIfExpired(t.Context(), config.ProviderConfig{ID: "p", OAuthToken: freshToken("still-good")})
	require.NoError(t, err)
}

func TestRefreshTokenIfExpired_Expired_Refreshes(t *testing.T) {
	newToken := freshToken("new-access")
	co := authTestCoordinator(t,
		withExchangeToken(fakeExchange(newToken, nil)),
		withProvider(func(p *config.ProviderConfig) { p.OAuthToken = expiredToken() }),
	)
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	require.True(t, providerCfg.OAuthToken.IsExpired())

	err := co.refreshTokenIfExpired(t.Context(), providerCfg)
	require.NoError(t, err)

	got, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	require.Equal(t, newToken.AccessToken, got.OAuthToken.AccessToken,
		"an expired token must be replaced by the refreshed one")
}

// ---------------------------------------------------------------------------
// retryAfterUnauthorized
// ---------------------------------------------------------------------------

func TestRetryAfterUnauthorized_OAuthSuccess(t *testing.T) {
	newToken := freshToken("new-access")
	co := authTestCoordinator(t,
		withExchangeToken(fakeExchange(newToken, nil)),
		withProvider(func(p *config.ProviderConfig) { p.OAuthToken = expiredToken() }),
	)
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)

	err := co.retryAfterUnauthorized(t.Context(), providerCfg)
	require.NoError(t, err)

	got, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	require.Equal(t, newToken.AccessToken, got.OAuthToken.AccessToken)
}

// TestRetryAfterUnauthorized_OAuthRevoked_WithNotifier_WaitsThenSucceeds
// covers the branch where the refresh token is revoked and a notifier is
// available: retryAfterUnauthorized must publish a TypeReAuthenticate
// notification and block in waitForInteractiveReauth until the interactive
// re-auth signal arrives, rather than surfacing the error immediately.
//
// SignalAuthComplete is called before retryAfterUnauthorized so the wait
// resolves without blocking (see its doc comment: a signal with no waiter
// registered yet pre-creates and closes the channel) - this keeps the test
// deterministic with no sleeps or timing races.
func TestRetryAfterUnauthorized_OAuthRevoked_WithNotifier_WaitsThenSucceeds(t *testing.T) {
	notifier := &recordingNotifier{}
	co := authTestCoordinator(t,
		withNotify(notifier),
		withExchangeToken(fakeExchange(nil, revokedExchangeErr)),
		withProvider(func(p *config.ProviderConfig) { p.OAuthToken = expiredToken() }),
	)
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)

	co.credentials.SignalAuthComplete(authProviderID)

	err := co.retryAfterUnauthorized(t.Context(), providerCfg)
	require.NoError(t, err, "once the interactive re-auth signal arrives, retryAfterUnauthorized must return nil so the caller retries")
	require.Equal(t, 1, notifier.count("", notify.TypeReAuthenticate),
		"a revoked refresh token with a notifier available must publish exactly one re-authenticate notification")
}

// TestRetryAfterUnauthorized_OAuthRevoked_Headless_ReturnsError pins the
// exact opposite of the WithNotifier case above using the same revoked
// error: with no notifier (c.notify == nil, e.g. a headless run),
// retryAfterUnauthorized must return the original error instead of
// blocking on interactive re-authentication that nothing can drive.
func TestRetryAfterUnauthorized_OAuthRevoked_Headless_ReturnsError(t *testing.T) {
	co := authTestCoordinator(t,
		withExchangeToken(fakeExchange(nil, revokedExchangeErr)),
		withProvider(func(p *config.ProviderConfig) { p.OAuthToken = expiredToken() }),
	)
	require.Nil(t, co.notify, "this test only means something headless")
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)

	err := co.retryAfterUnauthorized(t.Context(), providerCfg)
	require.Error(t, err)
	var exchangeErr *oauth.TokenExchangeError
	require.True(t, errors.As(err, &exchangeErr), "the original TokenExchangeError must be preserved in the error chain")
	require.True(t, exchangeErr.IsRefreshTokenRevoked())
}

func TestRetryAfterUnauthorized_AWSAuthRefresh_Headless_ReturnsErrNoInteractiveAuth(t *testing.T) {
	co := authTestCoordinator(t)
	require.Nil(t, co.notify)
	providerCfg := config.ProviderConfig{ID: authProviderID, AWSAuthRefresh: "aws sso login"}

	err := co.retryAfterUnauthorized(t.Context(), providerCfg)
	require.ErrorIs(t, err, errNoInteractiveAuth)
}

func TestRetryAfterUnauthorized_APIKeyTemplate_Refreshes(t *testing.T) {
	t.Setenv("SENNIT_TEST_AUTH_API_KEY", "resolved-key-123")
	co := authTestCoordinator(t,
		withProvider(func(p *config.ProviderConfig) {
			p.APIKeyTemplate = "$SENNIT_TEST_AUTH_API_KEY"
		}),
	)
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	require.Contains(t, providerCfg.APIKeyTemplate, "$")

	err := co.retryAfterUnauthorized(t.Context(), providerCfg)
	require.NoError(t, err)

	got, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	require.Equal(t, "resolved-key-123", got.APIKey,
		"a $-templated API key must be re-resolved and applied to the live provider config")
}

func TestRetryAfterUnauthorized_NoRefreshMechanism_NoOp(t *testing.T) {
	t.Parallel()
	c := &coordinator{}
	providerCfg := config.ProviderConfig{ID: "plain", APIKey: "static-key"}
	err := c.retryAfterUnauthorized(t.Context(), providerCfg)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// waitForInteractiveReauth
// ---------------------------------------------------------------------------

// TestWaitForInteractiveReauth_SurvivesCallerCancellationUntilSignaled pins
// two behaviors at once: cancelling the caller's context while the wait is
// in flight must NOT be what unblocks it (it uses a detached context with
// its own timeout - see auth.go), and once the interactive re-auth signal
// does arrive, waitForInteractiveReauth must check the ORIGINAL
// (now-cancelled) context and surface its error rather than proceeding to
// retry.
func TestWaitForInteractiveReauth_SurvivesCallerCancellationUntilSignaled(t *testing.T) {
	co := authTestCoordinator(t)
	const providerID = "revoked-provider"
	ctx, cancel := context.WithCancel(t.Context())

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- co.waitForInteractiveReauth(ctx, providerID)
	}()

	cancel()

	// The wait must still be blocked: nothing has called SignalAuthComplete
	// yet, and cancelling ctx must not be what wakes it (waitForInteractiveReauth
	// waits on a context.WithoutCancel(ctx) derivative). This is not a timing
	// guess - as long as no signal has fired, WaitForTokenChange cannot
	// return, so the absence of a result here is deterministic, not racy.
	select {
	case err := <-resultCh:
		t.Fatalf("waitForInteractiveReauth returned early (err=%v) after only the caller context was cancelled; "+
			"it must keep waiting for the interactive re-auth signal", err)
	case <-time.After(200 * time.Millisecond):
	}

	co.credentials.SignalAuthComplete(providerID)

	select {
	case err := <-resultCh:
		require.ErrorIs(t, err, context.Canceled,
			"after the signal arrives, waitForInteractiveReauth must check the ORIGINAL context and surface its cancellation instead of retrying")
	case <-time.After(5 * time.Second):
		t.Fatal("waitForInteractiveReauth did not return within the deadline after SignalAuthComplete")
	}
}

func TestWaitForInteractiveReauth_SuccessUpdatesModelsAndReturnsNil(t *testing.T) {
	co := authTestCoordinator(t)
	const providerID = authProviderID
	// Pre-signal so WaitForTokenChange resolves immediately - see the
	// WithNotifier retry test above for why this is deterministic.
	co.credentials.SignalAuthComplete(providerID)

	err := co.waitForInteractiveReauth(t.Context(), providerID)
	require.NoError(t, err)
}

func TestWaitForInteractiveReauth_UpdateModelsErrorPropagates(t *testing.T) {
	co := authTestCoordinator(t)
	const providerID = authProviderID
	co.credentials.SignalAuthComplete(providerID)

	// Break model selection so the post-signal UpdateModels rebuild fails
	// deterministically, without touching any I/O.
	co.cfg.OverridePreferredModel(config.SelectedModel{})

	err := co.waitForInteractiveReauth(t.Context(), providerID)
	require.ErrorIs(t, err, errModelNotSelected)
}

// ---------------------------------------------------------------------------
// makeAuthRefreshCallback
// ---------------------------------------------------------------------------

func TestMakeAuthRefreshCallback_NilWhenNoRefreshMechanism(t *testing.T) {
	co := authTestCoordinator(t)
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	require.Nil(t, providerCfg.OAuthToken)
	require.Empty(t, providerCfg.AWSAuthRefresh)

	cb := co.makeAuthRefreshCallback(providerCfg, nil)
	require.Nil(t, cb, "a provider with no OAuth token, AWS refresh command, or templated API key has nothing to refresh")
}

func TestMakeAuthRefreshCallback_RefreshesAndStoresRecompiledRuntime(t *testing.T) {
	newToken := freshToken("new-access")
	co := authTestCoordinator(t,
		withExchangeToken(fakeExchange(newToken, nil)),
		withProvider(func(p *config.ProviderConfig) { p.OAuthToken = expiredToken() }),
	)
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)

	initial, err := co.runtimeFor(t.Context())
	require.NoError(t, err)
	active := newActiveRuntime(initial)

	cb := co.makeAuthRefreshCallback(providerCfg, active)
	require.NotNil(t, cb)

	require.NoError(t, cb(t.Context(), nil))

	require.NotSame(t, initial, active.load(),
		"a successful refresh must recompile the runtime and store it on active")
	got, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)
	require.Equal(t, newToken.AccessToken, got.OAuthToken.AccessToken)
}

func TestMakeAuthRefreshCallback_NilActive_DoesNotPanic(t *testing.T) {
	newToken := freshToken("new-access")
	co := authTestCoordinator(t,
		withExchangeToken(fakeExchange(newToken, nil)),
		withProvider(func(p *config.ProviderConfig) { p.OAuthToken = expiredToken() }),
	)
	providerCfg, ok := co.cfg.Config().Providers.Get(authProviderID)
	require.True(t, ok)

	cb := co.makeAuthRefreshCallback(providerCfg, nil)
	require.NotNil(t, cb)
	require.NoError(t, cb(t.Context(), nil))
}
