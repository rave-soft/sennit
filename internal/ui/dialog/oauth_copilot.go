package dialog

import (
	"context"
	"fmt"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/workspace"
)

// CopilotProviderID is the provider this dialog signs in to, spelled out
// here for the same reason CodexProviderID is: the sign-in flow lives
// behind the workspace and this package does not import the vendor
// package that owns the id.
const CopilotProviderID = "copilot"

func NewOAuthCopilot(
	com *common.Common,
	isOnboarding bool,
	provider catwalk.Provider,
	model *config.SelectedModel,
	forceNewAccount bool,
) (*OAuth, tea.Cmd) {
	return newOAuth(com, isOnboarding, provider, model, &OAuthCopilot{com: com, proxy: configuredCopilotProxy(com)}, forceNewAccount)
}

// configuredCopilotProxy asks the workspace what proxy Copilot is already
// configured with. It is called from NewOAuthCopilot, which runs on the
// Update goroutine, and never again afterward: initiateAuth and
// startPolling run off tea.Cmds on their own goroutines, and reading
// config there would race with the dialog — see internal/ui/AGENTS.md's
// rule on capturing fields by value instead of the struct itself.
func configuredCopilotProxy(com *common.Common) string {
	// Common carries no workspace in tests, so its absence is a
	// legitimate "nothing configured" here.
	if com == nil || com.Workspace == nil {
		return ""
	}
	return com.Workspace.OAuthConfiguredProxy(CopilotProviderID)
}

type OAuthCopilot struct {
	com *common.Common
	// proxy is snapshot once at construction (see configuredCopilotProxy)
	// and never written again: initiateAuth/startPolling run off the
	// Update goroutine as tea.Cmds, and writing it from there would race
	// with a read from another — see internal/ui/AGENTS.md's rule on
	// capturing fields by value.
	proxy string

	// flow, initiateCancel, pollCancel and stopped are all touched from
	// tea.Cmd goroutines (initiateAuth sets flow/initiateCancel,
	// startPolling reads flow and sets pollCancel, stopPolling reads and
	// clears all of it) rather than from Update, so they need a lock of
	// their own — mirrors OAuthCodex's mu/stopped.
	//
	// stopped is what makes esc during "Initializing" safe. Without it, the
	// device-code request's own goroutine would write the device code
	// regardless of whether the dialog is still open, so a late result
	// after the dialog is dismissed and reopened would land in the new
	// instance (ActionInitiateOAuth is addressed by the constant OAuthID,
	// not by dialog instance) and could start a poll with a stale or, if
	// the timing flips, an altogether absent device code.
	mu             sync.Mutex
	flow           workspace.OAuthFlow
	initiateCancel func()
	pollCancel     func()
	stopped        bool
}

var (
	_ OAuthProvider  = (*OAuthCopilot)(nil)
	_ oauthProxyUser = (*OAuthCopilot)(nil)
)

func (m *OAuthCopilot) name() string {
	return "GitHub Copilot"
}

// currentProxy implements [oauthProxyUser]; see OAuthCodex.currentProxy.
// Copilot's own CompleteOAuth ignores it today, but passing it keeps every
// sign-in's completion shaped the same way.
func (m *OAuthCopilot) currentProxy() string {
	return m.proxy
}

func (m *OAuthCopilot) initiateAuth() tea.Msg {
	ctx, cancel := context.WithCancel(m.com.Context())
	defer cancel()

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.initiateCancel = cancel
	m.mu.Unlock()

	// The device-code request's own timeout lives with the flow, in the
	// workspace implementation; this context only carries cancellation.
	result, flow, err := m.com.Workspace.StartOAuth(ctx, CopilotProviderID, m.proxy)

	m.mu.Lock()
	m.initiateCancel = nil
	stopped := m.stopped
	m.mu.Unlock()
	if stopped {
		// Dismissed while the request was in flight. The dialog that would
		// have received this is gone — dropping it here is what keeps a
		// late result from landing in whatever OAuth dialog opens next.
		if flow != nil {
			flow.Cancel()
		}
		return nil
	}

	if err != nil {
		return ActionOAuthErrored{Error: err}
	}

	m.mu.Lock()
	m.flow = flow
	m.mu.Unlock()

	return ActionInitiateOAuth{
		DeviceCode:      result.DeviceCode,
		UserCode:        result.UserCode,
		VerificationURL: result.VerificationURL,
		ExpiresIn:       result.ExpiresIn,
		Interval:        result.Interval,
	}
}

func (m *OAuthCopilot) startPolling(deviceCode string, expiresIn int) tea.Cmd {
	m.mu.Lock()
	flow := m.flow
	if flow == nil {
		m.mu.Unlock()
		// Nothing to poll for — either initiateAuth hasn't landed yet or
		// this dialog never started its own flow. Returning here instead
		// of falling through avoids polling with no device code at all.
		return func() tea.Msg {
			return ActionOAuthErrored{Error: fmt.Errorf("copilot sign-in was not started")}
		}
	}
	ctx, cancel := context.WithCancel(m.com.Context())
	m.pollCancel = cancel
	m.mu.Unlock()
	return func() tea.Msg {
		token, err := flow.Wait(ctx)
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
	initiateCancel, pollCancel, flow := m.initiateCancel, m.pollCancel, m.flow
	m.initiateCancel, m.pollCancel, m.flow = nil, nil, nil
	m.mu.Unlock()

	if initiateCancel != nil {
		initiateCancel()
	}
	if pollCancel != nil {
		pollCancel()
	}
	if flow != nil {
		flow.Cancel()
	}
	return nil
}
