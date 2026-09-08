package key

import (
	"fmt"
	"unicode"

	bubbleskey "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

type (
	Binding    = bubbleskey.Binding
	BindingOpt = bubbleskey.BindingOpt
	Help       = bubbleskey.Help
)

var (
	NewBinding   = bubbleskey.NewBinding
	WithKeys     = bubbleskey.WithKeys
	WithHelp     = bubbleskey.WithHelp
	WithDisabled = bubbleskey.WithDisabled
)

func Matches[K fmt.Stringer](k K, bindings ...Binding) bool {
	if bubbleskey.Matches(k, bindings...) {
		return true
	}

	msg, ok := any(k).(tea.KeyMsg)
	if !ok {
		return false
	}
	physical, ok := physicalKeystroke(msg.Key())
	if !ok {
		return false
	}
	for _, binding := range bindings {
		if !binding.Enabled() {
			continue
		}
		for _, shortcut := range binding.Keys() {
			if shortcut == physical {
				return true
			}
		}
	}
	return false
}

func MatchesString[K fmt.Stringer](k K, shortcut string) bool {
	if shortcut == "" {
		return false
	}
	return Matches(k, NewBinding(WithKeys(shortcut)))
}

func physicalKeystroke(key tea.Key) (string, bool) {
	if key.BaseCode == 0 || key.BaseCode > unicode.MaxRune || !unicode.IsPrint(key.BaseCode) {
		return "", false
	}

	code := unicode.ToLower(key.BaseCode)
	mod := key.Mod
	if mod&(tea.ModCtrl|tea.ModAlt|tea.ModMeta|tea.ModHyper|tea.ModSuper) == 0 {
		shifted := mod&tea.ModShift != 0
		capsLocked := mod&tea.ModCapsLock != 0
		if shifted != capsLocked {
			return string(unicode.ToUpper(code)), true
		}
		return string(code), true
	}

	canonical := tea.Key{Code: code, Mod: mod}
	return canonical.Keystroke(), true
}
