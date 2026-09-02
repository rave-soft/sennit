package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (b *runtimeBuilder) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig, cred providerstate.Provider, port runtimeOperationPort) error {
	if cred.OAuthToken == nil || !cred.OAuthToken.IsExpired() {
		return nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	return b.refreshOAuth2Token(ctx, providerCfg, port)
}

// retryAfterUnauthorized attempts to refresh credentials after an auth error
// and returns nil if the request should be retried. For OAuth providers whose
// refresh token is revoked, and for Bedrock providers whose AWS SSO session
// has expired, it triggers interactive re-authentication and blocks until the
// user completes it (or the context is cancelled).
func (b *runtimeBuilder) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig, cred providerstate.Provider, port runtimeOperationPort) error {
	switch {
	case cred.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		if err := b.refreshOAuth2Token(ctx, providerCfg, port); err != nil {
			// If the refresh token was revoked, trigger interactive
			// re-auth and wait for the user to complete it.
			var exchangeErr *oauth.TokenExchangeError
			if b.notify != nil && errors.As(err, &exchangeErr) && exchangeErr.IsRefreshTokenRevoked() {
				slog.Info("Refresh token revoked, waiting for re-authentication", "provider", providerCfg.ID)
				b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
					Type:       notify.TypeReAuthenticate,
					ProviderID: providerCfg.ID,
				})
				return b.waitForInteractiveReauth(ctx, providerCfg.ID, port)
			}
			return err
		}
		return nil
	case providerCfg.AWSAuthRefresh != "":
		return b.refreshAWSCredentials(ctx, providerCfg, port)
	case strings.Contains(cred.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return b.refreshApiKeyTemplate(ctx, providerCfg, cred, port)
	default:
		return nil
	}
}

// errNoInteractiveAuth is returned by an OnAuthRefresh callback when a
// provider needs interactive re-authentication but no notifier is available
// to drive it (e.g. headless runs). Returning it surfaces the original auth
// error rather than retrying.
var errNoInteractiveAuth = errors.New("interactive authentication unavailable")

// waitForInteractiveReauth blocks until interactive re-authentication for the
// provider completes (signalled via SignalAuthComplete) or the context is
// cancelled, then rebuilds models so the next attempt picks up fresh
// credentials. Returns nil when the caller should retry.
func (b *runtimeBuilder) waitForInteractiveReauth(ctx context.Context, providerID string, port runtimeOperationPort) error {
	agent, inputs := port.agent, port.inputs
	// Use a detached context with a generous timeout so the wait survives
	// agent run cancellation. The user needs time to complete browser-based
	// authentication.
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer waitCancel()
	slog.Info("Blocking on WaitForTokenChange", "provider", providerID)
	if waitErr := b.credentials.WaitForTokenChange(waitCtx, providerID); waitErr != nil {
		slog.Info("WaitForTokenChange returned error", "provider", providerID, "error", waitErr)
		return waitErr
	}
	// If the original context was cancelled during the wait, fantasy's retry
	// would fail immediately, so surface the cancellation instead.
	if ctx.Err() != nil {
		slog.Warn("Original context cancelled during auth wait, cannot retry",
			"provider", providerID, "ctx_err", ctx.Err())
		return ctx.Err()
	}
	// Rebuild models so ModelProvider picks up the fresh credentials.
	if agent == nil {
		return nil
	}
	if updateErr := b.UpdateModels(waitCtx, agent, inputs); updateErr != nil {
		slog.Error("Failed to update models after re-authentication", "error", updateErr)
		return updateErr
	}
	slog.Info("Models updated, returning nil to retry", "provider", providerID)
	return nil
}

// buildProviderHTTPClient returns an OnAuthRefresh callback for fantasy that
// delegates to the builder's existing credential refresh logic. Returns
// nil if no refresh mechanism is configured for the provider. If active is
// non-nil, it is refreshed with the recompiled runtime after a successful
// credential refresh; pass nil when there is no active runtime to track.
func (b *runtimeBuilder) makeAuthRefreshCallback(providerCfg config.ProviderConfig, cred providerstate.Provider, active *activeRuntime, port runtimeOperationPort) func(context.Context, *fantasy.ProviderError) error {
	inputs := port.inputs
	if cred.OAuthToken == nil &&
		!strings.Contains(cred.APIKeyTemplate, "$") &&
		providerCfg.AWSAuthRefresh == "" {
		return nil
	}
	return func(ctx context.Context, _ *fantasy.ProviderError) error {
		if err := b.retryAfterUnauthorized(ctx, providerCfg, cred, port); err != nil {
			return err
		}
		if active != nil {
			runtime, err := b.runtimeFor(ctx, inputs)
			if err != nil {
				return err
			}
			active.store(runtime)
		}
		return nil
	}
}

// makeSubAgentAuthRefreshCallback is makeAuthRefreshCallback's counterpart
// for a delegation whose model differs from the coordinator's own (a
// custom agent's own "provider/model-id", or merely a cheaper model on the
// same OAuth provider). makeAuthRefreshCallback stores b.runtimeFor's
// result into active after a refresh, but that always rebuilds the
// coder/top-level runtime; modelProvider (turn.go) only adopts what is in
// active when both the provider AND the model match the turn's own, so for
// any sub-agent running a different model that store was silently a
// no-op - the retry kept using the pre-refresh provider instance built
// from t.model's stale credential, hit 401 again, and fantasy only
// refreshes once per pass, so the delegation died on an expiry the
// top-level agent recovers from cleanly. This variant rebuilds a runtime
// scoped to model (the sub-agent's own, already resolved by buildAgentModel
// in buildAgent) instead, so what lands in active
// actually matches what modelProvider is comparing against.
func (b *runtimeBuilder) makeSubAgentAuthRefreshCallback(providerCfg config.ProviderConfig, cred providerstate.Provider, model Model, active *activeRuntime, port runtimeOperationPort) func(context.Context, *fantasy.ProviderError) error {
	if cred.OAuthToken == nil &&
		!strings.Contains(cred.APIKeyTemplate, "$") &&
		providerCfg.AWSAuthRefresh == "" {
		return nil
	}
	return func(ctx context.Context, _ *fantasy.ProviderError) error {
		if err := b.retryAfterUnauthorized(ctx, providerCfg, cred, port); err != nil {
			return err
		}
		if active != nil {
			runtime, err := b.buildSubAgentRuntime(ctx, model)
			if err != nil {
				return err
			}
			active.store(runtime)
		}
		return nil
	}
}

// buildSubAgentRuntime rebuilds model's provider/language-model pair against
// the current config (i.e. after a credential refresh has landed a new
// token or key), the same way buildAgentModel does for
// a fresh delegation build. It re-reads providerCfg and providerCredentials
// from the live config store rather than trusting values captured before
// the refresh, since those are exactly what the refresh just replaced.
// Deliberately minimal: the only field a sub-agent's OnAuthRefresh path
// reads back out of the stored compiledRuntime is .model (see
// modelProvider in turn.go and summarize's ModelProvider in usage.go) -
// tools and system prompt are not rebuilt here, unlike runtimeFor's
// coder-runtime construction, because nothing on this path consults them.
func (b *runtimeBuilder) buildSubAgentRuntime(ctx context.Context, model Model) (*compiledRuntime, error) {
	cfg := b.cfg.Config()
	providerCfg, ok := cfg.Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}
	providerCredentials, ok := cfg.RuntimeProvider(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}
	rebuilt, err := b.buildModel(ctx, providerCfg, model.ModelCfg, true)
	if err != nil {
		return nil, err
	}
	return &compiledRuntime{model: rebuilt, providerCfg: providerCfg, providerCredentials: providerCredentials}, nil
}

func (b *runtimeBuilder) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig, port runtimeOperationPort) error {
	agent, inputs := port.agent, port.inputs
	if err := b.credentials.RefreshOAuthToken(ctx, config.ScopeGlobal, providerCfg.ID); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if agent == nil {
		return nil
	}
	return b.UpdateModels(ctx, agent, inputs)
}

func (b *runtimeBuilder) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig, cred providerstate.Provider, port runtimeOperationPort) error {
	agent, inputs := port.agent, port.inputs
	newAPIKey, err := b.cfg.Resolve(cred.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	if err := b.cfg.UpdateProviderCredentials(providerCfg.ID, newAPIKey, cred.OAuthToken); err != nil {
		return err
	}

	if agent == nil {
		return nil
	}
	return b.UpdateModels(ctx, agent, inputs)
}

// refreshAWSCredentials runs the provider's configured AWS SSO refresh
// command (e.g. "aws sso login") on the machine that makes the Bedrock
// calls, streaming the verification URL to the UI for display, then rebuilds
// models so the AWS SDK re-reads the refreshed credentials. It returns nil to
// signal that the failed request should be retried.
//
// The command runs here, in the coordinator, rather than in the UI dialog so
// the refreshed credentials land where the model calls are made.
func (b *runtimeBuilder) refreshAWSCredentials(ctx context.Context, providerCfg config.ProviderConfig, port runtimeOperationPort) error {
	agent, inputs := port.agent, port.inputs
	if b.notify == nil {
		return errNoInteractiveAuth
	}
	slog.Info("AWS credentials expired, running refresh command",
		"provider", providerCfg.ID, "command", providerCfg.AWSAuthRefresh)

	// Open the dialog immediately so the user sees progress even before the
	// command prints its verification URL.
	b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
		Type:         notify.TypeAWSSSOAuth,
		ProviderID:   providerCfg.ID,
		AWSSOCommand: providerCfg.AWSAuthRefresh,
	})

	// Detach from the turn's context (with a generous timeout) so cancelling
	// the turn doesn't kill an in-progress browser login.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), awsSSORefreshTimeout)
	defer cancel()

	runErr := b.runAWSAuthRefresh(runCtx, providerCfg)

	result := notify.Notification{Type: notify.TypeAWSSSOAuthResult, ProviderID: providerCfg.ID}
	if runErr != nil {
		result.Message = runErr.Error()
	}
	b.notify.Publish(pubsub.CreatedEvent, result)

	if runErr != nil {
		slog.Error("AWS SSO refresh command failed", "provider", providerCfg.ID, "error", runErr)
		return runErr
	}
	// If the turn's context was cancelled while the command ran, fantasy's
	// retry would fail immediately, so surface the cancellation instead.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Rebuild models so the AWS SDK credential chain re-reads the refreshed
	// SSO cache on the next attempt.
	b.invalidateRuntime(runCtx, "aws_auth_refresh", func() bool { return true })
	if agent == nil {
		slog.Info("AWS SSO refresh complete, no top-level agent to update", "provider", providerCfg.ID)
		return nil
	}
	if err := b.UpdateModels(runCtx, agent, inputs); err != nil {
		slog.Error("Failed to update models after AWS SSO refresh", "provider", providerCfg.ID, "error", err)
		return err
	}
	slog.Info("AWS SSO refresh complete, retrying request", "provider", providerCfg.ID)
	return nil
}

// runAWSAuthRefresh executes the refresh command, publishing the SSO
// verification URL to the UI as soon as it appears in the output and
// returning any failure with captured stderr for context.
func (b *runtimeBuilder) runAWSAuthRefresh(ctx context.Context, providerCfg config.ProviderConfig) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", providerCfg.AWSAuthRefresh)
	cmd.Dir = b.cfg.WorkingDir()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	// Drain stdout and stderr concurrently so a command that fills one pipe
	// buffer before closing the other can't deadlock. Both are scanned for
	// the verification URL; stderr is also captured for error detail.
	var (
		stderrBuf bytes.Buffer
		mu        sync.Mutex // Guards the single-shot URL publish across goroutines.
		urlSent   bool
	)
	publishURL := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if urlSent {
			return
		}
		if url := extractAWSSSOURL(line); url != "" {
			urlSent = true
			// Second phase of the two-part publish: the dialog is already
			// open from refreshAWSCredentials; this fills in the URL on it.
			b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
				Type:         notify.TypeAWSSSOAuth,
				ProviderID:   providerCfg.ID,
				AWSSOCommand: providerCfg.AWSAuthRefresh,
				AWSSOURL:     url,
			})
		}
	}

	var wg sync.WaitGroup
	var scanErrs [2]error
	scan := func(index int, name string, r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(nil, awsSSOOutputLineLimit)
		for scanner.Scan() {
			publishURL(scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			scanErrs[index] = fmt.Errorf("read AWS auth refresh %s: %w", name, err)
			_, _ = io.Copy(io.Discard, r)
		}
	}
	wg.Add(2)
	go scan(0, "stdout", stdout)
	go scan(1, "stderr", io.TeeReader(stderrPipe, &stderrBuf))
	wg.Wait()

	waitErr := cmd.Wait()
	var stderrErr error
	if stderr := strings.TrimSpace(stderrBuf.String()); stderr != "" && waitErr != nil {
		stderrErr = fmt.Errorf("%w: %s", waitErr, stderr)
		waitErr = nil
	}
	var scanErr error
	for _, err := range scanErrs {
		scanErr = errors.Join(scanErr, err)
	}
	return errors.Join(waitErr, stderrErr, scanErr)
}
