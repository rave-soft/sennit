package dialog

import (
	"context"
	"fmt"
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
	com        *common.Common
	deviceCode *copilot.DeviceCode
	cancelFunc func()
	// proxy is snapshot once at construction (see configuredCopilotProxy)
	// and never written again: initiateAuth/startPolling run off the
	// Update goroutine as tea.Cmds, and writing it from there would race
	// with a read from another — see internal/ui/AGENTS.md's rule on
	// capturing fields by value.
	proxy string
}

var _ OAuthProvider = (*OAuthCopilot)(nil)

func (m *OAuthCopilot) name() string {
	return "GitHub Copilot"
}

func (m *OAuthCopilot) initiateAuth() tea.Msg {
	ctx, cancel := context.WithTimeout(m.com.Context(), 30*time.Second)
	defer cancel()

	deviceCode, err := copilot.RequestDeviceCode(ctx, m.proxy)
	if err != nil {
		return ActionOAuthErrored{Error: fmt.Errorf("failed to initiate device auth: %w", err)}
	}

	m.deviceCode = deviceCode

	return ActionInitiateOAuth{
		DeviceCode:      deviceCode.DeviceCode,
		UserCode:        deviceCode.UserCode,
		VerificationURL: deviceCode.VerificationURI,
		ExpiresIn:       deviceCode.ExpiresIn,
		Interval:        deviceCode.Interval,
	}
}

func (m *OAuthCopilot) startPolling(deviceCode string, expiresIn int) tea.Cmd {
	ctx, cancel := context.WithCancel(m.com.Context())
	m.cancelFunc = cancel
	// Snapshot the device code: the poll below runs off the Update
	// goroutine, and m.deviceCode is reassigned when a new flow starts.
	device := m.deviceCode
	proxy := m.proxy
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
	if m.cancelFunc != nil {
		m.cancelFunc()
	}
	return nil
}
