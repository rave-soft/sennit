package model

import (
	"charm.land/bubbles/v2/key"
)

// ShortHelp implements [help.KeyMap].
func (m *UI) ShortHelp() []key.Binding {
	var binds []key.Binding
	k := &m.keyMap

	// When an inline editor is active, show its help.
	if m.activeInline != nil {
		return m.activeInline.ShortHelp()
	}

	commands := k.Commands
	if m.focus == uiFocusEditor && m.editor.textarea.Value() == "" {
		commands.SetHelp("/ or "+bindingShortcut(k.Commands), "commands")
	}

	switch m.state {
	case uiInitialize:
		binds = append(binds, k.Quit)
	case uiChat:
		// Show cancel binding if agent is busy.
		if m.isAgentBusy() {
			cancelBinding := k.Chat.Cancel
			if m.isCanceling {
				cancelBinding.SetHelp("esc", "press again to cancel")
			} else if len(m.wsCache.promptQueueCache.Value) > 0 {
				cancelBinding.SetHelp("esc", "clear queue")
			}
			binds = append(binds, cancelBinding)
		}

		// Show child-session navigation regardless of focus: the point is
		// discoverability of how to get back to the parent.
		if m.viewingChildSession() {
			binds = append(binds, k.Chat.ExitChildSession)
			if m.sess.childSessionSiblingCount() > 1 {
				binds = append(binds, k.Chat.PrevChildSession, k.Chat.NextChildSession)
			}
		}

		binds = append(
			binds,
			commands,
			k.Models,
		)

		switch m.focus {
		case uiFocusEditor:
			binds = append(
				binds,
				k.Editor.Newline,
			)
		case uiFocusMain:
			binds = append(
				binds,
				k.Chat.UpDown,
				k.Chat.UpDownOneItem,
				k.Chat.PageUp,
				k.Chat.PageDown,
				k.Chat.Copy,
			)
			if _, _, ok := m.chat.SelectedNestedToolContainer(); ok {
				binds = append(binds, k.Chat.EnterChildSession)
			}
		}
	default:
		// TODO: other states
		// if m.sess.current == nil {
		// no session selected
		binds = append(
			binds,
			commands,
			k.Models,
			k.Editor.Newline,
		)
	}

	binds = append(
		binds,
		k.Quit,
		k.Help,
	)

	return binds
}

// FullHelp implements [help.KeyMap].
func (m *UI) FullHelp() [][]key.Binding {
	// When an inline editor is active, show its help.
	if m.activeInline != nil {
		return [][]key.Binding{m.activeInline.ShortHelp()}
	}

	var binds [][]key.Binding
	k := &m.keyMap
	help := k.Help
	help.SetHelp(bindingShortcut(k.Help), "less")
	hasAttachments := len(m.editor.attachments.List()) > 0
	hasSession := m.hasSession()
	commands := k.Commands
	if m.focus == uiFocusEditor && m.editor.textarea.Value() == "" {
		commands.SetHelp("/ or "+bindingShortcut(k.Commands), "commands")
	}

	switch m.state {
	case uiInitialize:
		binds = append(binds,
			[]key.Binding{
				k.Quit,
			})
	case uiChat:
		// Show cancel binding if agent is busy.
		if m.isAgentBusy() {
			cancelBinding := k.Chat.Cancel
			if m.isCanceling {
				cancelBinding.SetHelp("esc", "press again to cancel")
			} else if len(m.wsCache.promptQueueCache.Value) > 0 {
				cancelBinding.SetHelp("esc", "clear queue")
			}
			binds = append(binds, []key.Binding{cancelBinding})
		}

		mainBinds := []key.Binding{}
		mainBinds = append(
			mainBinds,
			commands,
			k.Models,
			k.Sessions,
			k.ToggleYolo,
		)
		if hasSession {
			mainBinds = append(mainBinds, k.Chat.NewSession)
		}

		binds = append(binds, mainBinds)

		// Show child-session navigation regardless of focus: the point is
		// discoverability of how to get back to the parent.
		if m.viewingChildSession() {
			childBinds := []key.Binding{k.Chat.ExitChildSession}
			if m.sess.childSessionSiblingCount() > 1 {
				childBinds = append(childBinds, k.Chat.PrevChildSession, k.Chat.NextChildSession)
			}
			binds = append(binds, childBinds)
		}

		switch m.focus {
		case uiFocusEditor:
			editorBinds := []key.Binding{
				k.Editor.Newline,
				k.Editor.MentionFile,
				k.Editor.Commands,
				k.Editor.OpenEditor,
				k.Editor.ScrollPageUp,
				k.Editor.ScrollPageDown,
			}
			if currentModelSupportsImages(m.com) {
				editorBinds = append(editorBinds, k.Editor.AddImage, k.Editor.PasteImage)
			}
			binds = append(binds, editorBinds)
			if hasAttachments {
				binds = append(
					binds,
					[]key.Binding{
						k.Editor.AttachmentDeleteMode,
						k.Editor.DeleteAllAttachments,
						k.Editor.Escape,
					},
				)
			}
		case uiFocusMain:
			binds = append(
				binds,
				[]key.Binding{
					k.Chat.UpDown,
					k.Chat.UpDownOneItem,
					k.Chat.PageUp,
					k.Chat.PageDown,
				},
				[]key.Binding{
					k.Chat.HalfPageUp,
					k.Chat.HalfPageDown,
					k.Chat.Home,
					k.Chat.End,
				},
				[]key.Binding{
					k.Chat.Copy,
					k.Chat.ClearHighlight,
				},
			)
			if _, _, ok := m.chat.SelectedNestedToolContainer(); ok {
				binds = append(binds, []key.Binding{k.Chat.EnterChildSession})
			}
		}
	default:
		if m.sess.current == nil {
			// no session selected
			binds = append(
				binds,
				[]key.Binding{
					commands,
					k.Models,
					k.Sessions,
					k.ToggleYolo,
				},
			)
			editorBinds := []key.Binding{
				k.Editor.Newline,
				k.Editor.MentionFile,
				k.Editor.Commands,
				k.Editor.OpenEditor,
			}
			if currentModelSupportsImages(m.com) {
				editorBinds = append(editorBinds, k.Editor.AddImage, k.Editor.PasteImage)
			}
			binds = append(binds, editorBinds)
			if hasAttachments {
				binds = append(
					binds,
					[]key.Binding{
						k.Editor.AttachmentDeleteMode,
						k.Editor.DeleteAllAttachments,
						k.Editor.Escape,
					},
				)
			}
		}
	}

	binds = append(
		binds,
		[]key.Binding{
			help,
			k.Quit,
		},
	)

	return binds
}
