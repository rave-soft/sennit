package dialog

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// ActionClose is a message to close the current dialog.
type ActionClose struct{}

// ActionBatch carries several actions produced by a single message, for the
// cases where one keystroke means two things — moving through the theme
// picker both previews a palette and feeds the filter input. The handler
// applies them in order.
type ActionBatch struct {
	Actions []Action
}

// batchActions collapses actions into one: nil when nothing is left, the
// action itself when only one is, and an [ActionBatch] otherwise. Callers
// can pass nils freely.
func batchActions(actions ...Action) Action {
	kept := make([]Action, 0, len(actions))
	for _, a := range actions {
		if a != nil {
			kept = append(kept, a)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	default:
		return ActionBatch{Actions: kept}
	}
}

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
	// ActionSelectTheme is a message indicating a color palette has been
	// selected in the theme dialog. ID is a styles palette ID.
	ActionSelectTheme struct {
		ID string
	}
	// ActionPreviewTheme is sent as the selection moves through the theme
	// dialog: the palette is only highlighted, not chosen. The UI paints
	// itself in it so the list is a preview rather than a guess, and puts
	// the previous one back if the dialog closes without a choice.
	ActionPreviewTheme struct {
		ID string
	}
	ActionPermissionResponse struct {
		Permission permission.PermissionRequest
		Action     PermissionAction
	}
	// ActionRunCustomCommand is a message to run a custom command.
	ActionRunCustomCommand struct {
		Content   string
		Arguments []workspace.Argument
		Args      map[string]string // Actual argument values
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
		Arguments   []workspace.Argument
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
	// ActionAPIKeySaved carries the outcome of the async
	// SetProviderAPIKey call kicked off when the user confirms a verified
	// API key. It is deliberately NOT handled by handleDialogMsg's own
	// switch, so the generic "unhandled Action round-trips to the front
	// dialog" mechanism (see ActionCustomProviderResult) delivers it
	// straight back into the still-open APIKeyInput dialog's HandleMsg,
	// which decides between ActionProviderConfigured and
	// ActionSelectModel, or reports the error.
	ActionAPIKeySaved struct {
		Err error
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
	// has been successfully configured and verified. It closes the open
	// provider dialogs, and during onboarding also auto-selects a model
	// for this provider and transitions into chat.
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
	// ActionOpenAccountEdit is sent when "e" is pressed on the accounts
	// list, to open [AccountForm] for the selected account.
	ActionOpenAccountEdit struct {
		ProviderID string
		Account    accounts.Account
		// Active is whether this account is providerID's current one —
		// forwarded to AccountForm so it can refuse to let the user
		// disable it (see AccountForm.submit).
		Active bool
	}
	// ActionSubmitAccountForm is sent when the account edit form is
	// submitted with valid input.
	ActionSubmitAccountForm struct {
		ProviderID string
		Account    accounts.Account
	}
	// ActionAccountFormResult carries the outcome of the async
	// UpdateAccount call kicked off by ActionSubmitAccountForm. Like
	// ActionCustomProviderResult, it is addressed back to the still-open
	// AccountForm dialog (see DialogID below), letting the form show a
	// save error without closing.
	ActionAccountFormResult struct {
		ProviderID string
		Err        error
	}
	// ActionAccountSaved is returned by AccountForm.HandleMsg once
	// ActionAccountFormResult reports success. The caller closes the form
	// and reloads the accounts list.
	ActionAccountSaved struct {
		ProviderID string
	}
	// ActionRequestAccountRemoval is sent when "d" is pressed on the
	// accounts list, to open a confirmation dialog before actually
	// removing the selected account.
	ActionRequestAccountRemoval struct {
		ProviderID string
		Account    accounts.Account
	}
	// ActionRemoveAccountConfirmed is returned once the user confirms
	// removing an account in [AccountRemoveConfirm]. The caller performs
	// the actual removal and reloads the accounts list.
	ActionRemoveAccountConfirmed struct {
		ProviderID string
		AccountID  string
	}
	// ActionAccountActivated is returned by Accounts.HandleMsg once the
	// async ActivateAccount call it kicked off (selecting a different
	// account from the list) succeeds. It carries ProviderID so the
	// caller can refresh anything cached that depends on which account
	// is now active (the sidebar's account-label cache — see
	// model/account_label.go) in addition to closing the dialog.
	ActionAccountActivated struct {
		ProviderID string
	}
	// ActionOpenProviderSettings is sent when "Provider settings…" is
	// chosen from the accounts list, to open [ProviderSettings] for the
	// dialog's provider.
	ActionOpenProviderSettings struct {
		ProviderID string
	}
	// ActionSubmitProviderSettings is sent when the provider settings
	// form is submitted with valid input. Rotation is nil for a provider
	// whose accounts.CapabilitiesOf(...).RotateOn is accounts.RotateNever
	// — there is nothing to save for it.
	ActionSubmitProviderSettings struct {
		ProviderID string
		Proxy      string
		Rotation   *config.RotationConfig
	}
	// ActionProviderSettingsResult carries the outcome of the async
	// SetProviderProxy/SetConfigField calls kicked off by
	// ActionSubmitProviderSettings. Like ActionAccountFormResult, it is
	// addressed back to the still-open ProviderSettings dialog (see
	// DialogID below), letting the form show a save error without
	// closing.
	ActionProviderSettingsResult struct {
		ProviderID string
		Err        error
	}
	// ActionProviderSettingsSaved is returned by ProviderSettings.HandleMsg
	// once ActionProviderSettingsResult reports success. The caller
	// closes the form.
	ActionProviderSettingsSaved struct {
		ProviderID string
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

// The async results below are addressed to the dialog that started them —
// see [DialogAddressed] for what that buys.

// DialogID implements [DialogAddressed].
func (ActionAPIKeySaved) DialogID() string { return APIKeyInputID }

// DialogID implements [DialogAddressed]. Without this, the verification
// result raised while another dialog (e.g. a permission prompt) opened on
// top of APIKeyInput would go to that top dialog instead, leaving
// APIKeyInput stuck in APIKeyInputStateVerifying — a state that also
// swallows every key, esc included — with restart the only way out.
func (ActionChangeAPIKeyState) DialogID() string { return APIKeyInputID }

// DialogID implements [DialogAddressed].
func (ActionCustomProviderResult) DialogID() string { return ProviderFormID }

// DialogID implements [DialogAddressed].
func (ActionAccountFormResult) DialogID() string { return AccountFormID }

// DialogID implements [DialogAddressed].
func (ActionProviderSettingsResult) DialogID() string { return ProviderSettingsID }

// DialogID implements [DialogAddressed].
func (ActionMCPAuthComplete) DialogID() string { return MCPAuthID }

// DialogID implements [DialogAddressed].
func (ActionMCPAuthErrored) DialogID() string { return MCPAuthID }

// DialogID implements [DialogAddressed].
func (ActionInitiateOAuth) DialogID() string { return OAuthID }

// DialogID implements [DialogAddressed].
func (ActionCompleteOAuth) DialogID() string { return OAuthID }

// DialogID implements [DialogAddressed].
func (ActionOAuthErrored) DialogID() string { return OAuthID }
