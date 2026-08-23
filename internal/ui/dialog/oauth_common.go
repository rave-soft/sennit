package dialog

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/spinner"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// The three OAuth-style dialogs (OAuth, MCPAuth, AWSSSO) share a look —
// a themed spinner, a gradient title, and a header/body/help layout —
// but their state machines are genuinely different: OAuth drives its
// own device-flow polling through an OAuthProvider and adds a proxy
// step; MCPAuth walks a list of pending servers one at a time,
// cancelling an in-flight authorization via context on close or skip;
// AWSSSO owns no async work at all, just displaying SetURL/Finish
// calls the coordinator makes as the refresh command it runs
// elsewhere progresses. Only the rendering scaffolding below is
// factored out — the flows themselves are not forced together.

// newOAuthSpinner returns the themed dot spinner shown by the
// OAuth-style auth dialogs while a flow is in progress.
func newOAuthSpinner(t *styles.Styles) spinner.Model {
	return spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(t.Dialog.OAuth.Spinner),
	)
}

// oauthDialogHeader renders an OAuth-style dialog's title with the
// shared gradient treatment, sized to width minus the title and view
// frame padding.
func oauthDialogHeader(t *styles.Styles, width int, title string) string {
	titleStyle := t.Dialog.Title
	dialogStyle := t.Dialog.View.Width(width)
	headerOffset := titleStyle.GetHorizontalFrameSize() + dialogStyle.GetHorizontalFrameSize()
	return common.DialogTitle(t, titleStyle.Render(title), width-headerOffset, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
}

// oauthDialogContent joins an OAuth-style dialog's header, body, and
// help footer into the dialog's content, in the layout all three
// share outside of any state that suppresses chrome (OAuth's
// Initializing/Saving states render only the body).
func oauthDialogContent(t *styles.Styles, h *help.Model, km help.KeyMap, header, inner string, innerWidth int) string {
	return strings.Join([]string{header, inner, renderDialogHelp(t, h, km, innerWidth)}, "\n")
}
