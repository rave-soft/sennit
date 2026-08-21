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
) (*OAuth, tea.Cmd) {
	return newOAuth(com, isOnboarding, provider, model, &OAuthCopilot{com: com})
}

type OAuthCopilot struct {
	com        *common.Common
	deviceCode *copilot.DeviceCode
	cancelFunc func()
}

var _ OAuthProvider = (*OAuthCopilot)(nil)

func (m *OAuthCopilot) name() string {
	return "GitHub Copilot"
}

func (m *OAuthCopilot) initiateAuth() tea.Msg {
	ctx, cancel := context.WithTimeout(m.com.Context(), 30*time.Second)
	defer cancel()

	deviceCode, err := copilot.RequestDeviceCode(ctx)
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
	return func() tea.Msg {
		token, err := copilot.PollForToken(ctx, m.deviceCode)
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
