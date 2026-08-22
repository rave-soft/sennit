package model

import (
	"runtime"
	"strings"

	"charm.land/bubbles/v2/key"
)

type KeyMap struct {
	Editor struct {
		SendMessage key.Binding
		OpenEditor  key.Binding
		Newline     key.Binding
		AddImage    key.Binding
		PasteImage  key.Binding
		MentionFile key.Binding
		Commands    key.Binding

		// Attachments key maps
		AttachmentDeleteMode key.Binding
		Escape               key.Binding
		DeleteAllAttachments key.Binding

		// History navigation
		HistoryPrev key.Binding
		HistoryNext key.Binding

		// Chat scrolling while the editor keeps focus.
		ScrollPageUp   key.Binding
		ScrollPageDown key.Binding
	}

	Chat struct {
		NewSession     key.Binding
		AddAttachment  key.Binding
		Cancel         key.Binding
		Tab            key.Binding
		Details        key.Binding
		TogglePills    key.Binding
		Down           key.Binding
		Up             key.Binding
		UpDown         key.Binding
		DownOneItem    key.Binding
		UpOneItem      key.Binding
		UpDownOneItem  key.Binding
		PageDown       key.Binding
		PageUp         key.Binding
		HalfPageDown   key.Binding
		HalfPageUp     key.Binding
		Home           key.Binding
		End            key.Binding
		Copy           key.Binding
		ClearHighlight key.Binding
		Expand         key.Binding
		ScrollLeft     key.Binding
		ScrollRight    key.Binding

		// Sub-agent session navigation.
		EnterChildSession key.Binding
		ExitChildSession  key.Binding
		PrevChildSession  key.Binding
		NextChildSession  key.Binding
	}

	Initialize struct {
		Yes,
		No,
		Enter,
		Switch key.Binding
	}

	// Global key maps
	Quit       key.Binding
	Help       key.Binding
	Commands   key.Binding
	Models     key.Binding
	Suspend    key.Binding
	Sessions   key.Binding
	Tab        key.Binding
	ToggleYolo key.Binding
	Threads    key.Binding
}

func DefaultKeyMap() KeyMap {
	return keyMapForPlatform("", nil)
}

func configuredKeyMap(goos string, overrides map[string][]string) KeyMap {
	return keyMapForPlatform(goos, overrides)
}

func keyMapForPlatform(goos string, overrides map[string][]string) KeyMap {
	km := KeyMap{
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		Help: key.NewBinding(
			key.WithKeys("ctrl+g"),
			key.WithHelp("ctrl+g", "more"),
		),
		Commands: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("ctrl+p", "commands"),
		),
		Models: key.NewBinding(
			key.WithKeys("ctrl+m", "ctrl+l"),
			key.WithHelp("ctrl+l", "models"),
		),
		Suspend: key.NewBinding(
			key.WithKeys("ctrl+z"),
			key.WithHelp("ctrl+z", "suspend"),
		),
		Sessions: key.NewBinding(
			key.WithKeys("ctrl+s"),
			key.WithHelp("ctrl+s", "sessions"),
		),
		Tab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", ""),
		),
		ToggleYolo: key.NewBinding(
			key.WithKeys("ctrl+y"),
			key.WithHelp("ctrl+y", "toggle yolo"),
		),
		Threads: key.NewBinding(
			key.WithKeys("ctrl+e"),
			key.WithHelp("ctrl+e", "threads"),
		),
	}

	km.Editor.SendMessage = key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "send"),
	)
	km.Editor.OpenEditor = key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("ctrl+o", "open editor"),
	)
	km.Editor.Newline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
		// "ctrl+j" is a common keybinding for newline in many editors. If
		// the terminal supports "shift+enter", we substitute the help tex
		// to reflect that.
		key.WithHelp("ctrl+j", "newline"),
	)
	km.Editor.AddImage = key.NewBinding(
		key.WithKeys("ctrl+f"),
		key.WithHelp("ctrl+f", "add image"),
	)
	km.Editor.PasteImage = key.NewBinding(
		key.WithKeys("ctrl+v", "super+v"),
		key.WithHelp("ctrl+v", "paste images and text from clipboard"),
	)
	km.Editor.MentionFile = key.NewBinding(
		key.WithKeys("@"),
		key.WithHelp("@", "mention file"),
	)
	km.Editor.Commands = key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "commands"),
	)
	km.Editor.AttachmentDeleteMode = key.NewBinding(
		key.WithKeys("ctrl+r"),
		key.WithHelp("ctrl+r+{i}", "delete attachment at index i"),
	)
	km.Editor.Escape = key.NewBinding(
		key.WithKeys("esc", "alt+esc"),
		key.WithHelp("esc", "cancel delete mode"),
	)
	km.Editor.DeleteAllAttachments = key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("ctrl+r+r", "delete all attachments"),
	)
	km.Editor.HistoryPrev = key.NewBinding(
		key.WithKeys("up"),
	)
	km.Editor.HistoryNext = key.NewBinding(
		key.WithKeys("down"),
	)
	// Page keys scroll the conversation without stealing focus from the
	// editor; the chat's own "b"/"f"/space aliases stay out of these
	// bindings, since those are ordinary characters while typing.
	km.Editor.ScrollPageUp = key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("pgup", "page up"),
	)
	km.Editor.ScrollPageDown = key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("pgdn", "page down"),
	)

	km.Chat.NewSession = key.NewBinding(
		key.WithKeys("ctrl+n"),
		key.WithHelp("ctrl+n", "new session"),
	)
	km.Chat.AddAttachment = key.NewBinding(
		key.WithKeys("ctrl+f"),
		key.WithHelp("ctrl+f", "add attachment"),
	)
	km.Chat.Cancel = key.NewBinding(
		key.WithKeys("esc", "alt+esc"),
		key.WithHelp("esc", "cancel"),
	)
	km.Chat.Tab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", ""),
	)
	km.Chat.Details = key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("ctrl+d", "toggle details"),
	)
	km.Chat.TogglePills = key.NewBinding(
		key.WithKeys("ctrl+t", "ctrl+space"),
		key.WithHelp("ctrl+t", "toggle todos"),
	)

	km.Chat.Down = key.NewBinding(
		key.WithKeys("down", "ctrl+j", "j"),
		key.WithHelp("↓", "down"),
	)
	km.Chat.Up = key.NewBinding(
		key.WithKeys("up", "ctrl+k", "k"),
		key.WithHelp("↑", "up"),
	)
	km.Chat.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑↓", "scroll"),
	)
	km.Chat.UpOneItem = key.NewBinding(
		key.WithKeys("shift+up", "K"),
		key.WithHelp("shift+↑", "up one item"),
	)
	km.Chat.DownOneItem = key.NewBinding(
		key.WithKeys("shift+down", "J"),
		key.WithHelp("shift+↓", "down one item"),
	)
	km.Chat.UpDownOneItem = key.NewBinding(
		key.WithKeys("shift+up", "shift+down"),
		key.WithHelp("shift+↑↓", "scroll one item"),
	)
	km.Chat.HalfPageDown = key.NewBinding(
		key.WithKeys("d"),
		key.WithHelp("d", "half page down"),
	)
	km.Chat.PageDown = key.NewBinding(
		key.WithKeys("pgdown", " ", "f"),
		key.WithHelp("f/pgdn", "page down"),
	)
	km.Chat.PageUp = key.NewBinding(
		key.WithKeys("pgup", "b"),
		key.WithHelp("b/pgup", "page up"),
	)
	km.Chat.HalfPageUp = key.NewBinding(
		key.WithKeys("u"),
		key.WithHelp("u", "half page up"),
	)
	km.Chat.Home = key.NewBinding(
		key.WithKeys("g", "home"),
		key.WithHelp("g", "home"),
	)
	km.Chat.End = key.NewBinding(
		key.WithKeys("G", "end"),
		key.WithHelp("G", "end"),
	)
	km.Chat.Copy = key.NewBinding(
		key.WithKeys("c", "y", "C", "Y"),
		key.WithHelp("c/y", "copy"),
	)
	km.Chat.ClearHighlight = key.NewBinding(
		key.WithKeys("esc", "alt+esc"),
		key.WithHelp("esc", "clear selection"),
	)
	km.Chat.Expand = key.NewBinding(
		key.WithKeys("space"),
		key.WithHelp("space", "expand/collapse"),
	)
	km.Chat.ScrollLeft = key.NewBinding(
		key.WithKeys("shift+left", "H"),
		key.WithHelp("shift+←/H", "scroll left"),
	)
	km.Chat.ScrollRight = key.NewBinding(
		key.WithKeys("shift+right", "L"),
		key.WithHelp("shift+→/L", "scroll right"),
	)
	// Subagent navigation lives on ctrl+arrows; the old alt+arrow keys
	// stay as hidden aliases so existing muscle memory keeps working.
	km.Chat.EnterChildSession = key.NewBinding(
		key.WithKeys("ctrl+down", "alt+down"),
		key.WithHelp("ctrl+↓", "enter subagent"),
	)
	km.Chat.ExitChildSession = key.NewBinding(
		key.WithKeys("ctrl+up", "alt+up"),
		key.WithHelp("ctrl+↑", "exit subagent"),
	)
	km.Chat.PrevChildSession = key.NewBinding(
		key.WithKeys("ctrl+left", "alt+left"),
		key.WithHelp("ctrl+←", "prev subagent"),
	)
	km.Chat.NextChildSession = key.NewBinding(
		key.WithKeys("ctrl+right", "alt+right"),
		key.WithHelp("ctrl+→", "next subagent"),
	)

	km.Initialize.Yes = key.NewBinding(
		key.WithKeys("y", "Y"),
		key.WithHelp("y", "yes"),
	)
	km.Initialize.No = key.NewBinding(
		key.WithKeys("n", "N", "esc", "alt+esc"),
		key.WithHelp("n", "no"),
	)
	km.Initialize.Switch = key.NewBinding(
		key.WithKeys("left", "right", "tab"),
		key.WithHelp("tab", "switch"),
	)
	km.Initialize.Enter = key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "select"),
	)

	bindings := km.bindings()
	if goos == "darwin" {
		for _, binding := range bindings {
			keys := binding.Keys()
			for i, value := range keys {
				keys[i] = strings.Replace(value, "ctrl+", "super+", 1)
			}
			binding.SetKeys(uniqueStrings(keys)...)
			help := binding.Help()
			binding.SetHelp(strings.ReplaceAll(help.Key, "ctrl+", "super+"), help.Desc)
		}
	}
	for action, keys := range overrides {
		binding := bindings[action]
		if binding == nil || len(keys) == 0 {
			continue
		}
		keys = uniqueStrings(keys)
		binding.SetKeys(keys...)
		help := binding.Help()
		binding.SetHelp(formatShortcut(keys[0]), help.Desc)
	}
	deleteModeKey := bindingKey(km.Editor.AttachmentDeleteMode)
	if deleteModeKey != "" {
		help := km.Editor.AttachmentDeleteMode.Help()
		km.Editor.AttachmentDeleteMode.SetHelp(deleteModeKey+"+{i}", help.Desc)
		if deleteAllKey := bindingKey(km.Editor.DeleteAllAttachments); deleteAllKey != "" {
			help = km.Editor.DeleteAllAttachments.Help()
			km.Editor.DeleteAllAttachments.SetHelp(deleteModeKey+"+"+deleteAllKey, help.Desc)
		}
	}

	return km
}

func (k *KeyMap) bindings() map[string]*key.Binding {
	return map[string]*key.Binding{
		"quit":                          &k.Quit,
		"help":                          &k.Help,
		"commands":                      &k.Commands,
		"models":                        &k.Models,
		"suspend":                       &k.Suspend,
		"sessions":                      &k.Sessions,
		"tab":                           &k.Tab,
		"toggle_yolo":                   &k.ToggleYolo,
		"threads":                       &k.Threads,
		"editor.send_message":           &k.Editor.SendMessage,
		"editor.open_editor":            &k.Editor.OpenEditor,
		"editor.newline":                &k.Editor.Newline,
		"editor.add_image":              &k.Editor.AddImage,
		"editor.paste_image":            &k.Editor.PasteImage,
		"editor.mention_file":           &k.Editor.MentionFile,
		"editor.commands":               &k.Editor.Commands,
		"editor.attachment_delete_mode": &k.Editor.AttachmentDeleteMode,
		"editor.escape":                 &k.Editor.Escape,
		"editor.delete_all_attachments": &k.Editor.DeleteAllAttachments,
		"editor.history_prev":           &k.Editor.HistoryPrev,
		"editor.history_next":           &k.Editor.HistoryNext,
		"editor.scroll_page_up":         &k.Editor.ScrollPageUp,
		"editor.scroll_page_down":       &k.Editor.ScrollPageDown,
		"chat.new_session":              &k.Chat.NewSession,
		"chat.add_attachment":           &k.Chat.AddAttachment,
		"chat.cancel":                   &k.Chat.Cancel,
		"chat.tab":                      &k.Chat.Tab,
		"chat.details":                  &k.Chat.Details,
		"chat.toggle_pills":             &k.Chat.TogglePills,
		"chat.down":                     &k.Chat.Down,
		"chat.up":                       &k.Chat.Up,
		"chat.up_down":                  &k.Chat.UpDown,
		"chat.down_one_item":            &k.Chat.DownOneItem,
		"chat.up_one_item":              &k.Chat.UpOneItem,
		"chat.up_down_one_item":         &k.Chat.UpDownOneItem,
		"chat.page_down":                &k.Chat.PageDown,
		"chat.page_up":                  &k.Chat.PageUp,
		"chat.half_page_down":           &k.Chat.HalfPageDown,
		"chat.half_page_up":             &k.Chat.HalfPageUp,
		"chat.home":                     &k.Chat.Home,
		"chat.end":                      &k.Chat.End,
		"chat.copy":                     &k.Chat.Copy,
		"chat.clear_highlight":          &k.Chat.ClearHighlight,
		"chat.expand":                   &k.Chat.Expand,
		"chat.scroll_left":              &k.Chat.ScrollLeft,
		"chat.scroll_right":             &k.Chat.ScrollRight,
		"chat.enter_child_session":      &k.Chat.EnterChildSession,
		"chat.exit_child_session":       &k.Chat.ExitChildSession,
		"chat.prev_child_session":       &k.Chat.PrevChildSession,
		"chat.next_child_session":       &k.Chat.NextChildSession,
		"initialize.yes":                &k.Initialize.Yes,
		"initialize.no":                 &k.Initialize.No,
		"initialize.enter":              &k.Initialize.Enter,
		"initialize.switch":             &k.Initialize.Switch,
	}
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func formatShortcut(value string) string {
	parts := strings.Split(value, "+")
	for i, part := range parts {
		switch part {
		case "up":
			parts[i] = "↑"
		case "down":
			parts[i] = "↓"
		case "left":
			parts[i] = "←"
		case "right":
			parts[i] = "→"
		}
	}
	return strings.Join(parts, "+")
}

func bindingShortcut(binding key.Binding) string {
	keys := binding.Keys()
	if len(keys) == 0 {
		return ""
	}
	return formatShortcut(keys[0])
}

func bindingKey(binding key.Binding) string {
	keys := binding.Keys()
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func (m *UI) exitChildSessionShortcut() string {
	shortcut := bindingKey(m.keyMap.Chat.ExitChildSession)
	if shortcut != "" {
		return shortcut
	}
	goos := m.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	return bindingKey(configuredKeyMap(goos, nil).Chat.ExitChildSession)
}
