package styles

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// quickStyleOpts is the palette of colors used by quickStyle to simplify the
// process of building a theme.
type quickStyleOpts struct {
	// Brand.
	primary   color.Color
	secondary color.Color
	accent    color.Color
	keyword   color.Color

	// Default foreground and background colors.
	fgBase color.Color
	bgBase color.Color
	// Markdown panel background, slightly raised above bgBase.
	bgMarkdown color.Color
	bgHover    color.Color

	// Low-contrast dividers, separators, and rule lines.
	separator color.Color

	fgSubtle     color.Color
	fgMoreSubtle color.Color
	fgMostSubtle color.Color
	fgMarkdown   color.Color

	// Contrast pairings: foregrounds designed to sit on top of a
	// matching background role.
	onPrimary color.Color // foreground on primary backgrounds.

	bgMostVisible  color.Color
	bgLessVisible  color.Color
	bgLeastVisible color.Color

	// Statuses.
	destructive       color.Color
	error             color.Color
	warning           color.Color
	warningSubtle     color.Color
	attention         color.Color
	busy              color.Color
	info              color.Color
	infoMoreSubtle    color.Color
	infoMostSubtle    color.Color
	success           color.Color
	successMoreSubtle color.Color
	successMostSubtle color.Color

	// ANSI 16-color palette. These remap the basic terminal colors that
	// programs emit (e.g. bang-mode shell output) onto legible, on-brand
	// colors instead of leaving them to the user's terminal defaults.
	// Normal intensity.
	ansiBlack   color.Color
	ansiRed     color.Color
	ansiGreen   color.Color
	ansiYellow  color.Color
	ansiBlue    color.Color
	ansiMagenta color.Color
	ansiCyan    color.Color
	ansiWhite   color.Color
	// Bright intensity.
	ansiBrightBlack   color.Color
	ansiBrightRed     color.Color
	ansiBrightGreen   color.Color
	ansiBrightYellow  color.Color
	ansiBrightBlue    color.Color
	ansiBrightMagenta color.Color
	ansiBrightCyan    color.Color
	ansiBrightWhite   color.Color

	// Syntax highlighting for markdown code blocks (chroma) and the few
	// markdown elements — links, images — that are colored like code.
	syntaxKeyword   color.Color
	syntaxType      color.Color
	syntaxBuiltin   color.Color
	syntaxTag       color.Color
	syntaxAttribute color.Color
	syntaxOperator  color.Color
	syntaxClass     color.Color
	syntaxString    color.Color
	syntaxDecorator color.Color
	syntaxPreproc   color.Color
	syntaxLink      color.Color

	// Diff line colors. diffview needs a foreground plus two background
	// shades per side: a dimmer one for the gutter (line number) and a
	// brighter one for the symbol and code. They are tokens rather than
	// literals so a theme can restyle diffs without editing quickStyle
	// -- see the "fully token-driven" rule in AGENTS.md.
	diffInsertFg    color.Color
	diffInsertBgDim color.Color
	diffInsertBg    color.Color
	diffDeleteFg    color.Color
	diffDeleteBgDim color.Color
	diffDeleteBg    color.Color
}

// quickStyle builds the default Styles (that is, the default theme, Sennit's
// own Steel & Teal palette — see palette.go for why it deliberately diverges
// from the stock charmtone neutrals) from a palette of semi-semanticly-named
// colors.
//
// The idea here is that you can do most of the work on a theme with quickStyle,
// then add overrides as needed.
//
// The build is split into one function per UI area (quickStyleEditor,
// quickStyleChat, ...) so no single function grows unwieldy. Every field of
// Styles must be assigned by exactly one of these — see TestQuickStyleFieldsAssignedOnce
// for the mechanical check.
func quickStyle(o quickStyleOpts) Styles {
	var (
		base   = lipgloss.NewStyle().Foreground(o.fgBase)
		muted  = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
		subtle = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
		s      Styles
	)

	quickStyleWorking(&s, o, base, muted, subtle)
	quickStyleEditor(&s, o, base, muted, subtle)
	quickStyleMarkdown(&s, o, base, muted, subtle)
	quickStyleQuietMarkdown(&s, o, base, muted, subtle)
	quickStyleDiff(&s, o, base, muted, subtle)
	quickStyleHeader(&s, o, base, muted, subtle)
	quickStyleTool(&s, o, base, muted, subtle)
	quickStyleButton(&s, o, base, muted, subtle)
	quickStyleRadio(&s, o, base, muted, subtle)
	quickStyleTab(&s, o, base, muted, subtle)
	quickStyleLogo(&s, o, base, muted, subtle)
	quickStyleSection(&s, o, base, muted, subtle)
	quickStyleThreads(&s, o, base, muted, subtle)
	quickStyleChildBanner(&s, o, base, muted, subtle)
	quickStyleInitialize(&s, o, base, muted, subtle)
	quickStyleResource(&s, o, base, muted, subtle)
	quickStyleLSP(&s, o, base, muted, subtle)
	quickStyleFiles(&s, o, base, muted, subtle)
	quickStyleSidebar(&s, o, base, muted, subtle)
	quickStyleModelInfo(&s, o, base, muted, subtle)
	quickStyleChat(&s, o, base, muted, subtle)
	quickStyleDialog(&s, o, base, muted, subtle)
	quickStyleStatus(&s, o, base, muted, subtle)

	return s
}
