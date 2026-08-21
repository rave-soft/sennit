package model

import (
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/dialog"
)

// widgets holds the stateful sub-components UI draws every frame and routes
// messages into: the dialog stack, the status/help line, the header, the
// chat list, and whichever inline editor (if any) currently replaces the
// textarea. They are grouped because they are all owned, constructed once
// in New, and referenced from nearly every corner of Update/Draw — unlike
// the state structs in ui.go (sessionState, layoutState, ...), which each
// belong to one narrower concern.
//
// Embedded anonymously (by value) so its fields keep promoting onto *UI
// unchanged (m.dialog, m.chat, ...): see the doc comment on UI for why
// value embedding matters here, same reasoning as appServices/appEvents in
// internal/app/app.go.
type widgets struct {
	dialog *dialog.Overlay
	status *Status
	header *header
	chat   *Chat

	// activeInline replaces the textarea when non-nil.
	activeInline dialog.InlineEditor
	// inlineCursor stores the cursor from the last inline editor Draw call,
	// used by the cursor positioning logic in ui.go.
	inlineCursor *tea.Cursor
}
