package model

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestDrawChatSeparatorsAboveAndBelowChat(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.updateLayoutAndSize()
	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChatSeparators(scr, u.lay.layout.editor)

	for _, y := range []int{u.lay.layout.editor.Min.Y - 1, u.lay.layout.editor.Max.Y} {
		for x := u.lay.layout.editor.Min.X; x < u.lay.layout.editor.Max.X; x++ {
			cell := scr.CellAt(x, y)
			require.NotNil(t, cell)
			require.Equal(t, styles.SectionSeparator, cell.Content, "missing chat separator at (%d,%d)", x, y)
		}
	}
}

func TestEditorHasReservedSeparatorRows(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	area := u.generateLayout(u.lay.width, u.lay.height)

	require.Equal(t, area.editor.Min.Y-1, area.main.Max.Y,
		"content must leave exactly one separator row before the editor")
	require.Equal(t, 1, area.status.Min.Y-area.editor.Max.Y,
		"exactly one separator row must remain below the editor")
}

// testMessageItem is a minimal chat item used to populate the chat list
// without pulling in full message rendering machinery.
type testMessageItem struct {
	id   string
	text string
}

func (m testMessageItem) ID() string           { return m.id }
func (m testMessageItem) Render(int) string    { return m.text }
func (m testMessageItem) RawRender(int) string { return m.text }
func (m testMessageItem) Version() uint64      { return 0 }
func (m testMessageItem) Finished() bool       { return true }

var _ chat.MessageItem = testMessageItem{}

// newTestUI builds a focused uiChat model with dynamic textarea sizing enabled.
// It intentionally keeps dependencies minimal so layout behavior can be tested
// in isolation.
func newTestUI() *UI {
	com := common.DefaultCommon(context.Background(), nil)

	ta := textarea.New()
	ta.SetStyles(com.Styles.Editor.Textarea)
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	ta.SetVirtualCursor(false)
	ta.DynamicHeight = true
	ta.MinHeight = TextareaMinHeight
	ta.MaxHeight = TextareaMaxHeight
	ta.Focus()

	u := &UI{
		com:    com,
		status: NewStatus(com, nil),
		chat:   NewChat(com, config.ScrollbarDefault),
		editor: editorState{textarea: ta},
		state:  uiChat,
		focus:  uiFocusEditor,
		lay: layoutState{
			width:  140,
			height: 45,
		},
		panel: sessionPanelState{
			hoveredPanelThread: -1,
		},
	}

	return u
}

func TestUpdateLayoutAndSize_EditorGrowthShrinksChat(t *testing.T) {
	t.Parallel()

	// Baseline layout at min textarea height.
	u := newTestUI()
	u.updateLayoutAndSize()

	initialEditorHeight := u.lay.layout.editor.Dy()
	initialChatHeight := u.lay.layout.main.Dy()

	// Increase textarea content enough to trigger growth, then run the
	// same resize hook used in the real update path.
	prevHeight := u.editor.textarea.Height()
	u.editor.textarea.SetValue(strings.Repeat("line\n", 8))
	u.editor.textarea.MoveToEnd()
	_ = u.handleTextareaHeightChange(prevHeight)

	if got := u.lay.layout.editor.Dy(); got <= initialEditorHeight {
		t.Fatalf("expected editor to grow: got %d, want > %d", got, initialEditorHeight)
	}

	if got := u.lay.layout.main.Dy(); got >= initialChatHeight {
		t.Fatalf("expected chat to shrink: got %d, want < %d", got, initialChatHeight)
	}
}

// TestEditorHeight_EmptyIsOneRowNoAttachments: an empty editor with no
// attachments must be exactly one row tall — no reserved blank line above
// (attachments strip) or below it. This is the "two extra lines" the
// editor used to always reserve (TextareaMinHeight=3, editorHeightMargin=2).
func TestEditorHeight_EmptyIsOneRowNoAttachments(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.updateLayoutAndSize()

	require.Equal(t, 1, u.lay.layout.editor.Dy())
	require.Equal(t, "", u.editor.textarea.Value())
}

// TestEditorHeight_GrowsWithLineCountUpToCap: adding lines of text must
// grow the editor's rendered height by one row per line, up to
// TextareaMaxHeight, after which the textarea scrolls internally instead
// of growing the layout further.
func TestEditorHeight_GrowsWithLineCountUpToCap(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.updateLayoutAndSize()
	require.Equal(t, 1, u.lay.layout.editor.Dy(), "baseline: empty editor is one row")

	prevHeight := u.editor.textarea.Height()
	u.editor.textarea.SetValue("line1\nline2\nline3")
	u.editor.textarea.MoveToEnd()
	_ = u.handleTextareaHeightChange(prevHeight)
	require.Equal(t, 3, u.lay.layout.editor.Dy(), "three lines of text must grow the editor to three rows")

	prevHeight = u.editor.textarea.Height()
	u.editor.textarea.SetValue(strings.Repeat("line\n", 30))
	u.editor.textarea.MoveToEnd()
	_ = u.handleTextareaHeightChange(prevHeight)
	require.Equal(t, TextareaMaxHeight, u.lay.layout.editor.Dy(),
		"growth must cap at TextareaMaxHeight; beyond that the textarea scrolls internally")
}

// TestEditorHeight_AttachmentsRowOnlyWhenPresent: the attachments strip
// must only take up a row when there's actually an attachment to show —
// adding one grows the editor by exactly one row, and clearing it shrinks
// back down.
func TestEditorHeight_AttachmentsRowOnlyWhenPresent(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.updateLayoutAndSize()
	require.Equal(t, 0, u.editorAttachmentsRowOffset(), "no attachments: no reserved row")
	heightNoAttachments := u.lay.layout.editor.Dy()

	u.editor.attachments.Update(message.Attachment{FileName: "a.txt", MimeType: "text/plain"})
	u.updateLayoutAndSize()
	require.Equal(t, 1, u.editorAttachmentsRowOffset())
	require.Equal(t, heightNoAttachments+1, u.lay.layout.editor.Dy(),
		"one attachment must add exactly one row")

	u.editor.attachments.Reset()
	u.updateLayoutAndSize()
	require.Equal(t, heightNoAttachments, u.lay.layout.editor.Dy(),
		"clearing attachments must give the row back to chat")
}

// TestRenderEditorView_NoAttachmentsRowWhenEmpty: renderEditorView must not
// prepend a blank attachments line (or a trailing blank margin line) when
// there are no attachments — the rendered view is exactly the textarea's
// own lines.
func TestRenderEditorView_NoAttachmentsRowWhenEmpty(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})

	out := u.renderEditorView(80)
	require.Equal(t, u.editor.textarea.View(), out)
}

func TestHandleTextareaHeightChange_FollowModeStaysAtBottom(t *testing.T) {
	t.Parallel()

	// Use enough messages to make the chat scrollable so AtBottom/Follow
	// assertions are meaningful.
	u := newTestUI()

	msgs := make([]chat.MessageItem, 0, 60)
	for i := range 60 {
		msgs = append(msgs, testMessageItem{
			id:   "m-" + strconv.Itoa(i),
			text: "message " + strconv.Itoa(i),
		})
	}
	u.chat.SetMessages(msgs...)
	u.updateLayoutAndSize()

	// Enter follow mode and verify we're anchored at the bottom first.
	u.chat.ScrollToBottom()
	if !u.chat.AtBottom() {
		t.Fatal("expected chat to start at bottom")
	}

	// Grow the editor; follow mode should keep the chat pinned to the end
	// even as the chat viewport shrinks.
	prevHeight := u.editor.textarea.Height()
	u.editor.textarea.SetValue(strings.Repeat("line\n", 10))
	u.editor.textarea.MoveToEnd()
	_ = u.handleTextareaHeightChange(prevHeight)

	if !u.chat.Follow() {
		t.Fatal("expected follow mode to remain enabled")
	}
	if !u.chat.AtBottom() {
		t.Fatal("expected chat to remain at bottom after editor resize in follow mode")
	}
}

func TestAutoExpandTodosIfReasonable(t *testing.T) {
	t.Parallel()

	t.Run("expands when terminal is tall enough and todos exist", func(t *testing.T) {
		t.Parallel()

		u := newTestUI()
		u.lay.height = 50
		u.sess.session = &session.Session{ID: "s1", Todos: []session.Todo{
			{Status: session.TodoStatusInProgress, Content: "do work"},
			{Status: session.TodoStatusPending, Content: "do more"},
		}}

		u.autoExpandTodosIfReasonable()

		if !u.panel.expanded {
			t.Fatal("expected todos to be expanded")
		}
	})

	t.Run("does not expand when terminal is too short", func(t *testing.T) {
		t.Parallel()

		u := newTestUI()
		u.lay.height = 30
		u.sess.session = &session.Session{ID: "s1", Todos: []session.Todo{
			{Status: session.TodoStatusInProgress, Content: "do work"},
		}}

		u.autoExpandTodosIfReasonable()

		if u.panel.expanded {
			t.Fatal("expected todos to stay collapsed when terminal height is below threshold")
		}
	})

	t.Run("does not expand when all todos are completed", func(t *testing.T) {
		t.Parallel()

		u := newTestUI()
		u.lay.height = 50
		u.sess.session = &session.Session{ID: "s1", Todos: []session.Todo{
			{Status: session.TodoStatusCompleted, Content: "done"},
		}}

		u.autoExpandTodosIfReasonable()

		if u.panel.expanded {
			t.Fatal("expected todos to stay collapsed when all todos are completed")
		}
	})

	t.Run("does not expand when already expanded", func(t *testing.T) {
		t.Parallel()

		u := newTestUI()
		u.lay.height = 50
		u.panel.expanded = true
		u.sess.session = &session.Session{ID: "s1", Todos: []session.Todo{
			{Status: session.TodoStatusInProgress, Content: "do work"},
		}}
		u.updateLayoutAndSize()

		u.autoExpandTodosIfReasonable()

		if !u.panel.expanded {
			t.Fatal("expected todos to stay expanded")
		}
	})

	t.Run("does not expand for prompt queue alone (queue is always visible now)", func(t *testing.T) {
		t.Parallel()

		u := newTestUI()
		u.lay.height = 50
		u.sess.session = &session.Session{ID: "s1", Todos: []session.Todo{}}
		u.wsCache.promptQueue = 2

		u.autoExpandTodosIfReasonable()

		if u.panel.expanded {
			t.Fatal("expected todos to stay collapsed: the queue no longer drives auto-expand")
		}
	})

	t.Run("does not expand when no session", func(t *testing.T) {
		t.Parallel()

		u := newTestUI()
		u.lay.height = 50
		u.sess.session = nil

		u.autoExpandTodosIfReasonable()

		if u.panel.expanded {
			t.Fatal("expected todos to stay collapsed when there is no session")
		}
	})
}
