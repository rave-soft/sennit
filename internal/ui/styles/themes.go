package styles

import "github.com/charmbracelet/x/exp/charmtone"

// Sennit's default brand colors, re-exported from the default palette for the
// few places that need a brand color outside the Styles graph (the CLI's
// version mark and `sennit dirs`, the generated notification icon's reference
// values). Those surfaces render before or outside the TUI, so they always
// speak the default scheme rather than the selected theme; everything drawn
// inside the TUI goes through Styles and follows the theme. See
// [Palette] for the palettes themselves.
var (
	BrandPrimary   = PaletteSteelTeal.Primary
	BrandSecondary = PaletteSteelTeal.Secondary
	BrandAccent    = PaletteSteelTeal.Accent
	BrandKeyword   = PaletteSteelTeal.Keyword

	BrandFg           = PaletteSteelTeal.Fg
	BrandFgSubtle     = PaletteSteelTeal.FgSubtle
	BrandFgMoreSubtle = PaletteSteelTeal.FgMoreSubtle
	BrandFgMostSubtle = PaletteSteelTeal.FgMostSubtle
	BrandFgMarkdown   = PaletteSteelTeal.FgMarkdown

	BrandBg            = PaletteSteelTeal.Bg
	BrandBgMarkdown    = PaletteSteelTeal.BgMarkdown
	BrandBgHover       = PaletteSteelTeal.BgHover
	BrandBgLeast       = PaletteSteelTeal.BgLeast
	BrandBgLess        = PaletteSteelTeal.BgLess
	BrandBgMost        = PaletteSteelTeal.BgMost
	BrandSeparator     = PaletteSteelTeal.Separator
	BrandOnPrimary     = PaletteSteelTeal.OnPrimary
	BrandAccentDeep    = PaletteSteelTeal.AccentDeep
	BrandNeutralBright = PaletteSteelTeal.NeutralBright

	BrandSuccess     = PaletteSteelTeal.Success
	BrandDestructive = PaletteSteelTeal.Destructive
	BrandError       = PaletteSteelTeal.Error
	BrandWarning     = PaletteSteelTeal.Warning
	BrandWarningSoft = PaletteSteelTeal.WarningSoft
	BrandAttention   = PaletteSteelTeal.Attention
)

// SennitDark returns Sennit's default dark theme.
func SennitDark() Styles {
	return Theme(DefaultThemeID)
}

// Theme returns the Styles for the palette with the given ID, falling back
// to the default palette when the ID is unknown (see [PaletteByID]).
func Theme(id string) Styles {
	return themeFromPalette(PaletteByID(id))
}

// themeFromPalette builds the full Styles graph from a palette. Everything
// theme-dependent reads p; nothing here hardcodes a color.
func themeFromPalette(p Palette) Styles {
	s := quickStyle(quickStyleOpts{
		primary:   p.Primary,
		secondary: p.Secondary,
		accent:    p.Accent,
		keyword:   p.Keyword,

		fgBase:       p.Fg,
		fgSubtle:     p.FgSubtle,
		fgMoreSubtle: p.FgMoreSubtle,
		fgMostSubtle: p.FgMostSubtle,
		fgMarkdown:   p.FgMarkdown,

		onPrimary: p.OnPrimary,

		bgBase:         p.Bg,
		bgMarkdown:     p.BgMarkdown,
		bgHover:        p.BgHover,
		bgLeastVisible: p.BgLeast,
		bgLessVisible:  p.BgLess,
		bgMostVisible:  p.BgMost,

		separator: p.Separator,

		destructive:   p.Destructive,
		error:         p.Error,
		warningSubtle: p.WarningSoft,
		warning:       p.Warning,
		attention:     p.Attention,
		busy:          p.Accent,
		info:          p.Accent,
		// "info" text is common enough that a fully saturated accent
		// everywhere would undo the restraint; the subtler steps step down
		// toward the neutrals instead of toward another hue.
		infoMoreSubtle: p.Primary,
		infoMostSubtle: p.AccentDeep,
		// `success` colors *text* (dialog titles, resource notes, link
		// text), where a green would read as decoration; it stays neutral
		// and the green below is spent on check marks alone.
		success:           p.FgSubtle,
		successMoreSubtle: p.FgMoreSubtle,
		successMostSubtle: p.FgMoreSubtle,

		// ANSI 16-color palette for remapping raw terminal output
		// (e.g. bang-mode shell commands) onto legible colors. These stay
		// close to what the named colors mean — a program emitting "red"
		// wants red — with only blue/cyan pulled onto the theme.
		ansiBlack:   p.BgLeast,
		ansiRed:     charmtone.Coral,
		ansiGreen:   charmtone.Guac,
		ansiYellow:  charmtone.Mustard,
		ansiBlue:    p.Primary,
		ansiMagenta: charmtone.Dolly,
		ansiCyan:    p.Accent,
		ansiWhite:   p.FgSubtle,

		ansiBrightBlack:   p.BgMost,
		ansiBrightRed:     charmtone.Tuna,
		ansiBrightGreen:   charmtone.Julep,
		ansiBrightYellow:  charmtone.Zest,
		ansiBrightBlue:    p.Keyword,
		ansiBrightMagenta: charmtone.Blush,
		ansiBrightCyan:    p.Secondary,
		ansiBrightWhite:   p.NeutralBright,

		syntaxKeyword:   p.SyntaxKeyword,
		syntaxType:      p.SyntaxType,
		syntaxBuiltin:   p.SyntaxBuiltin,
		syntaxTag:       p.SyntaxTag,
		syntaxAttribute: p.SyntaxAttribute,
		syntaxOperator:  p.SyntaxOperator,
		syntaxClass:     p.SyntaxClass,
		syntaxString:    p.SyntaxString,
		syntaxDecorator: p.SyntaxDecorator,
		syntaxPreproc:   p.SyntaxPreproc,
		syntaxLink:      p.SyntaxLink,
	})

	// Bang ! prompt: the accent carries it, on the bright neutral.
	s.Editor.PromptBangIconFocused = s.Editor.PromptBangIconFocused.
		Foreground(p.NeutralBright).
		Background(p.Accent)
	s.Editor.PromptBangDotsFocused = s.Editor.PromptBangDotsFocused.
		Foreground(p.Accent)
	s.Editor.PromptBangDotsBlurred = s.Editor.PromptBangDotsBlurred.
		Foreground(p.AccentDeep)

	// Shell bar/prompt.
	s.Messages.ShellBarFocused = s.Messages.ShellBarFocused.
		BorderForeground(p.Primary)
	s.Messages.ShellBarBlurred = s.Messages.ShellBarBlurred.
		BorderForeground(p.BgMost)
	s.Messages.ShellPrompt = s.Messages.ShellPrompt.
		Foreground(p.Accent)
	s.Messages.ShellPromptBlurred = s.Messages.ShellPromptBlurred.
		Foreground(p.Accent)
	s.Files.Additions = s.Files.Additions.Foreground(p.Success)

	// Check marks are green everywhere they appear — the one place the
	// scheme spends a second hue. A ✓ is a discrete semantic mark rather
	// than decoration: against muted or struck-through text a neutral check
	// reads as "also grayed out" instead of "done". Only the glyph carries
	// the color; the text around it stays as it was.
	// The "enabled" dot in resource lists (skills, MCP servers) is the same
	// semantic green, not the raw ANSI green quickStyle defaults it to —
	// that one is tuned for remapped shell output and reads as neon here.
	s.Resource.EnabledIcon = s.Resource.EnabledIcon.Foreground(p.Success)

	// A finished thread on the dashboard is marked with the same green as
	// a finished todo. Left on the default neutral it was pixel-identical
	// to idle, which is the one distinction that screen exists to make.
	s.Threads.StatusDone = s.Threads.StatusDone.Foreground(p.Success)

	s.ToolCallSuccess = s.ToolCallSuccess.Foreground(p.Success)
	s.Tool.IconSuccess = s.Tool.IconSuccess.Foreground(p.Success)
	s.Tool.JobIconSuccess = s.Tool.JobIconSuccess.Foreground(p.Success)
	s.Tool.TodoCompletedIcon = s.Tool.TodoCompletedIcon.Foreground(p.Success)
	s.Status.SuccessIndicator = s.Status.SuccessIndicator.Foreground(p.Success)

	return s
}
