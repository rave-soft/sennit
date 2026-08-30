package dialog

import (
	"context"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/rave-soft/sennit/internal/ui/common"
)

func NewOAuthCopilot(
	com *common.Common,
	isOnboarding bool,
	provider catwalk.Provider,
	model *config.SelectedModel,
	forceNewAccount bool,
) (*OAuth, tea.Cmd) {
	return newOAuth(com, isOnboarding, provider, model, &OAuthCopilot{com: com, proxy: configuredCopilotProxy(com)}, forceNewAccount)
}

// configuredCopilotProxy reads whatever proxy Copilot is already configured
// with. It is called from NewOAuthCopilot, which runs on the Update
// goroutine, and never again afterward: initiateAuth and startPolling run
// off tea.Cmds on their own goroutines, and reading config there would race
// with the dialog — see internal/ui/AGENTS.md's rule on capturing fields by
// value instead of the struct itself.
func configuredCopilotProxy(com *common.Common) string {
	// Common carries no workspace in tests, and Config panics on one, so
	// the absence of a config is a legitimate "nothing configured" here.
	if com == nil || com.Workspace == nil {
		return ""
	}
	cfg := com.Config()
	if cfg == nil {
		return ""
	}
	pc, ok := cfg.Providers.Get("copilot")
	if !ok {
		return ""
	}
	return pc.ProxyURL
}

type OAuthCopilot struct {
	com *common.Common
	// proxy is snapshot once at construction (see configuredCopilotProxy)
	// and never written again: initiateAuth/startPolling run off the
	// Update goroutine as tea.Cmds, and writing it from there would race
	// with a read from another — see internal/ui/AGENTS.md's rule on
	// capturing fields by value.
	proxy string

	// deviceCode, initiateCancel, pollCancel and stopped are all touched
	// from tea.Cmd goroutines (initiateAuth sets deviceCode/initiateCancel,
	// startPolling reads deviceCode and sets pollCancel, stopPolling reads
	// and clears all of it) rather than from Update, so they need a lock
	// of their own — mirrors OAuthCodex's mu/stopped.
	//
	// stopped is what makes esc during "Initializing" safe: previously the
	// device-code request ran on its own uncancellable 30s context and
	// wrote m.deviceCode from that goroutine regardless of whether the
	// dialog was still open, so a late result after the dialog was
	// dismissed and reopened landed in the new instance (ActionInitiateOAuth
	// is addressed by the constant OAuthID, not by dialog instance) and
	// could start a poll with a stale or, if the timing flipped, an
	// altogether absent device code.
	mu             sync.Mutex
	deviceCode     *copilot.DeviceCode
	initiateCancel func()
	pollCancel     func()
	stopped        bool
}

var _ OAuthProvider = (*OAuthCopilot)(nil)

func (m *OAuthCopilot) name() string {
	return "GitHub Copilot"
}

func (m *OAuthCopilot) initiateAuth() tea.Msg {
	ctx, cancel := context.WithTimeout(m.com.Context(), 30*time.Second)
	defer cancel()

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.initiateCancel = cancel
	m.mu.Unlock()

	deviceCode, err := copilot.RequestDeviceCode(ctx, m.proxy)

	m.mu.Lock()
	m.initiateCancel = nil
	stopped := m.stopped
	m.mu.Unlock()
	if stopped {
		// Dismissed while the request was in flight. The dialog that would
		// have received this is gone — dropping it here is what keeps a
		// late result from landing in whatever OAuth dialog opens next.
		return nil
	}

	if err != nil {
		return ActionOAuthErrored{Error: fmt.Errorf("failed to initiate device auth: %w", err)}
	}

	m.mu.Lock()
	m.deviceCode = deviceCode
	m.mu.Unlock()

	return ActionInitiateOAuth{
		DeviceCode:      deviceCode.DeviceCode,
		UserCode:        deviceCode.UserCode,
		VerificationURL: deviceCode.VerificationURI,
		ExpiresIn:       deviceCode.ExpiresIn,
		Interval:        deviceCode.Interval,
	}
}

func (m *OAuthCopilot) startPolling(deviceCode string, expiresIn int) tea.Cmd {
	m.mu.Lock()
	device := m.deviceCode
	if device == nil {
		m.mu.Unlock()
		// Nothing to poll for — either initiateAuth hasn't landed yet or
		// this dialog never started its own flow. Returning here instead
		// of falling through avoids the nil-deviceCode panic inside
		// copilot.PollForToken.
		return func() tea.Msg {
			return ActionOAuthErrored{Error: fmt.Errorf("copilot sign-in was not started")}
		}
	}
	ctx, cancel := context.WithCancel(m.com.Context())
	m.pollCancel = cancel
	proxy := m.proxy
	m.mu.Unlock()
	return func() tea.Msg {
		token, err := copilot.PollForToken(ctx, proxy, device)
		if err != nil {
			if ctx.Err() != nil {
				return nil // cancelled, don't report error.
			}
			return ActionOAuthErrored{Error: err}
		}

		return ActionCompleteOAuth{Token: token}
	}
}

func (m *OAuthCopilot) stopPolling() tea.Msg {
	m.mu.Lock()
	m.stopped = true
	initiateCancel, pollCancel := m.initiateCancel, m.pollCancel
	m.initiateCancel, m.pollCancel = nil, nil
	m.mu.Unlock()

	if initiateCancel != nil {
		initiateCancel()
	}
	if pollCancel != nil {
		pollCancel()
	}
	return nil
}
