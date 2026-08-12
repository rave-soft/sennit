package styles

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/charmtone"
)

// ThemeForProvider returns the Styles associated with the given provider
// ID. CharmtonePantera is currently the only theme, so every provider
// resolves to it.
func ThemeForProvider(_ string) Styles {
	return CharmtonePantera()
}

// CharmtonePantera returns the Charmtone dark theme. It's the default style
// for the UI.
func CharmtonePantera() Styles {
	// The scheme is deliberately restrained: everyday chrome (tool names,
	// status icons, hooks, todos) renders in neutrals and the muted
	// brand purple (Hazy) instead of saturated blue/green/yellow, so the
	// chat reads as monochrome-with-one-accent. Saturated colors are
	// reserved for rare, genuinely semantic states: errors, warnings,
	// destructive dialogs.
	s := quickStyle(quickStyleOpts{
		primary:   charmtone.Charple,
		secondary: charmtone.Dolly,
		accent:    charmtone.Hazy,
		keyword:   charmtone.Blush,

		// Text neutrals are custom cool (blue-tinted) grays rather than
		// the stock charmtone neutrals, which lean warm/purple — matched
		// to the originals in brightness (Sash/Smoke/Squid/Oyster) but
		// colder in hue.
		fgBase:       lipgloss.Color("#E4EBF3"),
		fgMoreSubtle: lipgloss.Color("#7E8B99"),
		fgSubtle:     lipgloss.Color("#AEBDCC"),
		fgMostSubtle: lipgloss.Color("#57626E"),
		fgMarkdown:   lipgloss.Color("#C7D2DE"),

		onPrimary: charmtone.Butter,

		bgBase:         lipgloss.Color("#0F0E13"),
		bgMarkdown:     lipgloss.Color("#201F27"),
		bgHover:        lipgloss.Color("#272630"),
		bgLeastVisible: charmtone.BBQ,
		bgLessVisible:  charmtone.Char,
		bgMostVisible:  charmtone.Iron,

		separator: charmtone.Char,

		destructive:       charmtone.Coral,
		error:             charmtone.Sriracha,
		warningSubtle:     charmtone.Zest,
		warning:           charmtone.Mustard,
		attention:         charmtone.Tang,
		busy:              charmtone.Hazy,
		info:              charmtone.Hazy,
		infoMoreSubtle:    charmtone.Larple,
		infoMostSubtle:    charmtone.Squid,
		success:           charmtone.Smoke,
		successMoreSubtle: charmtone.Squid,
		successMostSubtle: charmtone.Squid,

		// ANSI 16-color palette for remapping raw terminal output
		// (e.g. bang-mode shell commands) onto legible Charmtone colors.
		ansiBlack:   charmtone.BBQ,
		ansiRed:     charmtone.Coral,
		ansiGreen:   charmtone.Guac,
		ansiYellow:  charmtone.Mustard,
		ansiBlue:    charmtone.Charple,
		ansiMagenta: charmtone.Dolly,
		ansiCyan:    charmtone.Malibu,
		ansiWhite:   charmtone.Smoke,

		ansiBrightBlack:   charmtone.Iron,
		ansiBrightRed:     charmtone.Tuna,
		ansiBrightGreen:   charmtone.Julep,
		ansiBrightYellow:  charmtone.Zest,
		ansiBrightBlue:    charmtone.Guppy,
		ansiBrightMagenta: charmtone.Blush,
		ansiBrightCyan:    charmtone.Sardine,
		ansiBrightWhite:   charmtone.Salt,
	})

	// Bang ! prompt overrides - use Salt/Hazy/Larple colors.
	s.Editor.PromptBangIconFocused = s.Editor.PromptBangIconFocused.
		Foreground(charmtone.Salt).
		Background(charmtone.Hazy)
	s.Editor.PromptBangDotsFocused = s.Editor.PromptBangDotsFocused.
		Foreground(charmtone.Hazy)
	s.Editor.PromptBangDotsBlurred = s.Editor.PromptBangDotsBlurred.
		Foreground(charmtone.Larple)

	// Shell bar/prompt overrides - use Charple/Iron/Hazy colors.
	s.Messages.ShellBarFocused = s.Messages.ShellBarFocused.
		BorderForeground(charmtone.Charple)
	s.Messages.ShellBarBlurred = s.Messages.ShellBarBlurred.
		BorderForeground(charmtone.Iron)
	s.Messages.ShellPrompt = s.Messages.ShellPrompt.
		Foreground(charmtone.Hazy)
	s.Messages.ShellPromptBlurred = s.Messages.ShellPromptBlurred.
		Foreground(charmtone.Hazy)
	s.Files.Additions = s.Files.Additions.Foreground(charmtone.Guac)

	return s
}
