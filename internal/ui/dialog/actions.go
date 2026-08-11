package dialog

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/util"
)

// ActionClose is a message to close the current dialog.
type ActionClose struct{}

// ActionQuit is a message to quit the application.
type ActionQuit = tea.QuitMsg

// ActionOpenDialog is a message to open a dialog.
type ActionOpenDialog struct {
	DialogID string
}

// ActionSelectSession is a message indicating a session has been selected.
type ActionSelectSession struct {
	Session session.Session
}

// ActionSelectModel is a message indicating a model has been selected.
type ActionSelectModel struct {
	Provider       catwalk.Provider
	Model          config.SelectedModel
	ModelType      config.SelectedModelType
	ReAuthenticate bool
}

// Messages for commands
type (
	ActionNewSession              struct{}
	ActionToggleHelp              struct{}
	ActionToggleCompactMode       struct{}
	ActionToggleThinking          struct{}
	ActionTogglePills             struct{}
	ActionExternalEditor          struct{}
	ActionToggleYoloMode          struct{}
	ActionToggleNotifications     struct{}
	ActionSelectNotificationStyle struct {
		Style string
	}
	ActionToggleTransparentBackground struct{}
	ActionInitializeProject           struct{}
	// ActionOpenThreadsDashboard requests switching to the threads
	// dashboard screen (see internal/ui/model/root.go's screenDashboard).
	ActionOpenThreadsDashboard struct{}
	ActionSummarize            struct {
		SessionID string
	}
	// ActionSelectReasoningEffort is a message indicating a reasoning effort
	// has been selected.
	ActionSelectReasoningEffort struct {
		Effort string
	}
	ActionPermissionResponse struct {
		Permission permission.PermissionRequest
		Action     PermissionAction
	}
	// ActionRunCustomCommand is a message to run a custom command.
	ActionRunCustomCommand struct {
		Content   string
		Arguments []commands.Argument
		Args      map[string]string // Actual argument values
		Skill     *skills.Skill     // Set when this is a skill command
	}
	// ActionAttachSkill is sent when a skill is selected from the commands
	// dialog to be attached to the conversation as a markdown attachment.
	ActionAttachSkill struct {
		ID   string
		Name string
	}
	// ActionRunMCPPrompt is a message to run a custom command.
	ActionRunMCPPrompt struct {
		Title       string
		Description string
		PromptID    string
		ClientID    string
		Arguments   []commands.Argument
		Args        map[string]string // Actual argument values
	}
	// ActionEnableDockerMCP is a message to enable Docker MCP.
	ActionEnableDockerMCP struct{}
	// ActionDisableDockerMCP is a message to disable Docker MCP.
	ActionDisableDockerMCP struct{}
)

// Messages for MCP OAuth authentication dialog.
type (
	// ActionMCPAuthStarted is sent when the user approves authentication
	// for an MCP server. The UI should initiate the actual auth flow
	// using the provided context, which the dialog will cancel if the
	// user closes it.
	ActionMCPAuthStarted struct {
		Name string
		Ctx  context.Context
	}

	// ActionMCPAuthComplete is sent when MCP authentication succeeds.
	ActionMCPAuthComplete struct {
		Name string
	}

	// ActionMCPAuthErrored is sent when MCP authentication fails.
	ActionMCPAuthErrored struct {
		Name  string
		Error error
	}
)

// Messages for API key input dialog.
type (
	ActionChangeAPIKeyState struct {
		State APIKeyInputState
	}
)

// Messages for OAuth2 device flow dialog.
type (
	// ActionInitiateOAuth is sent when the device auth is initiated
	// successfully.
	ActionInitiateOAuth struct {
		DeviceCode      string
		UserCode        string
		ExpiresIn       int
		VerificationURL string
		Interval        int
	}

	// ActionCompleteOAuth is sent when the device flow completes successfully.
	ActionCompleteOAuth struct {
		Token *oauth.Token
	}

	// ActionOAuthErrored is sent when the device flow encounters an error.
	ActionOAuthErrored struct {
		Error error
	}
)

// Messages for the providers configuration dialog and custom provider form.
type (
	// ActionConfigureProvider is sent when a catalog provider is chosen from
	// the providers list dialog, to start authenticating/configuring it.
	ActionConfigureProvider struct {
		ProviderID string
	}
	// ActionOpenCustomProviderForm is sent when "Custom provider…" is chosen
	// from the providers list dialog.
	ActionOpenCustomProviderForm struct{}
	// ActionSubmitCustomProvider is sent when the custom provider form is
	// submitted with valid input.
	ActionSubmitCustomProvider struct {
		ID      string
		BaseURL string
		Type    string
		APIKey  string
	}
	// ActionProviderConfigured is sent once a provider (catalog or custom)
	// has been successfully configured and verified. Portion 1 only closes
	// the open dialogs; portion 2 will add auto-selecting a model for this
	// provider and transitioning into chat.
	ActionProviderConfigured struct {
		ProviderID string
	}
	// ActionCustomProviderResult carries the outcome of the async
	// save-config + model-discovery work kicked off by
	// ActionSubmitCustomProvider. It is deliberately NOT handled by
	// handleDialogMsg's own switch, so the generic "unhandled Action
	// round-trips to the front dialog" mechanism (see
	// ActionChangeAPIKeyState) delivers it straight back into the
	// still-open ProviderForm dialog's HandleMsg, letting the form show a
	// discovery error without closing, or hand back control to close on
	// success.
	ActionCustomProviderResult struct {
		ProviderID string
		Err        error
	}
)

// ActionCmd represents an action that carries a [tea.Cmd] to be passed to the
// Bubble Tea program loop.
type ActionCmd struct {
	Cmd tea.Cmd
}

// ActionFilePickerSelected is a message indicating a file has been selected in
// the file picker dialog.
type ActionFilePickerSelected struct {
	Path string
}

// Cmd returns a command that reads the file at path and sends a
// [message.Attachement] to the program.
func (a ActionFilePickerSelected) Cmd() tea.Cmd {
	path := a.Path
	if path == "" {
		return nil
	}
	return func() tea.Msg {
		isFileLarge, err := common.IsFileTooBig(path, common.MaxAttachmentSize)
		if err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("unable to read the image: %v", err),
			}
		}
		if isFileLarge {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  "file too large, max 5MB",
			}
		}

		attachment, err := common.AttachmentFromPath(path)
		if err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("unable to read the image: %v", err),
			}
		}

		return attachment
	}
}
