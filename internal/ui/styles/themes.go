package styles

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/charmtone"
)

// Braid's brand palette: cool steel neutrals with a single muted teal
// accent. Exported so the few places that need a brand color outside the
// Styles graph (the CLI's version mark, the generated notification icon's
// reference values) name the same colors instead of copying hexes.
//
// The scheme is deliberately restrained: everyday chrome (tool names,
// status icons, hooks, todos) renders in the neutrals and the teal accent
// rather than in saturated hues, so a screen reads as monochrome with one
// accent. Saturated colors are reserved for rare, genuinely semantic
// states: errors, warnings, destructive dialogs, and the green that marks
// something done.
var (
	// Brand.
	BrandPrimary   = lipgloss.Color("#4A8FA8") // steel teal
	BrandSecondary = lipgloss.Color("#6ECBD6") // aqua; the far end of the working gradient
	BrandAccent    = lipgloss.Color("#5FB3C4") // the one accent everyday chrome may use
	BrandKeyword   = lipgloss.Color("#7FA8C9") // slate blue

	// Neutrals. Cool (blue-tinted) rather than the stock charmtone
	// neutrals, which lean warm/purple.
	BrandFg           = lipgloss.Color("#E4EBF3")
	BrandFgSubtle     = lipgloss.Color("#AEBDCC")
	BrandFgMoreSubtle = lipgloss.Color("#7E8B99")
	BrandFgMostSubtle = lipgloss.Color("#57626E")
	BrandFgMarkdown   = lipgloss.Color("#C7D2DE")

	BrandBg            = lipgloss.Color("#0F1216")
	BrandBgMarkdown    = lipgloss.Color("#1A1F25")
	BrandBgHover       = lipgloss.Color("#222831")
	BrandBgLeast       = lipgloss.Color("#151A20")
	BrandBgLess        = lipgloss.Color("#1E242B")
	BrandBgMost        = lipgloss.Color("#2C343D")
	BrandSeparator     = lipgloss.Color("#232A32")
	BrandOnPrimary     = lipgloss.Color("#08131A")
	BrandAccentDeep    = lipgloss.Color("#3B6675")
	BrandNeutralBright = lipgloss.Color("#F2F6FA")

	// Syntax highlighting (markdown code blocks, chroma). Code is the one
	// place that genuinely needs several distinguishable hues, so this is
	// not reduced to the single accent — but it is built from the same cool
	// base, with two warm exceptions (strings and decorators) to keep
	// literals readable against all the blues.
	SyntaxKeyword   = lipgloss.Color("#7FA8C9")
	SyntaxType      = lipgloss.Color("#6ECBD6")
	SyntaxBuiltin   = lipgloss.Color("#8FD4DF")
	SyntaxTag       = lipgloss.Color("#86A5B8")
	SyntaxAttribute = lipgloss.Color("#5FB3C4")
	SyntaxOperator  = lipgloss.Color("#9AAAB8")
	SyntaxClass     = lipgloss.Color("#F2F6FA")
	SyntaxString    = lipgloss.Color("#8FB593")
	SyntaxDecorator = lipgloss.Color("#C9B25E")
	SyntaxPreproc   = lipgloss.Color("#C98F5E")
	SyntaxLink      = lipgloss.Color("#7FA8C9")

	// Semantic. The only saturated colors in the scheme.
	BrandSuccess     = lipgloss.Color("#6E9E7C") // sage green: "done", check marks
	BrandDestructive = lipgloss.Color("#D9705F")
	BrandError       = lipgloss.Color("#C4483A")
	BrandWarning     = lipgloss.Color("#C99A3E")
	BrandWarningSoft = lipgloss.Color("#D9A857")
	BrandAttention   = lipgloss.Color("#D98E4E")
)

// ThemeForProvider returns the Styles associated with the given provider
// ID. BraidDark is currently the only theme, so every provider resolves to
// it.
func ThemeForProvider(_ string) Styles {
	return BraidDark()
}

// BraidDark returns Braid's dark theme — the default style for the UI. See
// the brand palette above for the scheme's rationale.
func BraidDark() Styles {
	s := quickStyle(quickStyleOpts{
		primary:   BrandPrimary,
		secondary: BrandSecondary,
		accent:    BrandAccent,
		keyword:   BrandKeyword,

		fgBase:       BrandFg,
		fgSubtle:     BrandFgSubtle,
		fgMoreSubtle: BrandFgMoreSubtle,
		fgMostSubtle: BrandFgMostSubtle,
		fgMarkdown:   BrandFgMarkdown,

		onPrimary: BrandOnPrimary,

		bgBase:         BrandBg,
		bgMarkdown:     BrandBgMarkdown,
		bgHover:        BrandBgHover,
		bgLeastVisible: BrandBgLeast,
		bgLessVisible:  BrandBgLess,
		bgMostVisible:  BrandBgMost,

		separator: BrandSeparator,

		destructive:   BrandDestructive,
		error:         BrandError,
		warningSubtle: BrandWarningSoft,
		warning:       BrandWarning,
		attention:     BrandAttention,
		busy:          BrandAccent,
		info:          BrandAccent,
		// "info" text is common enough that a fully saturated accent
		// everywhere would undo the restraint; the subtler steps step down
		// toward the neutrals instead of toward another hue.
		infoMoreSubtle: BrandPrimary,
		infoMostSubtle: BrandAccentDeep,
		// `success` colors *text* (dialog titles, resource notes, link
		// text), where a green would read as decoration; it stays neutral
		// and the green below is spent on check marks alone.
		success:           BrandFgSubtle,
		successMoreSubtle: BrandFgMoreSubtle,
		successMostSubtle: BrandFgMoreSubtle,

		// ANSI 16-color palette for remapping raw terminal output
		// (e.g. bang-mode shell commands) onto legible colors. These stay
		// close to what the named colors mean — a program emitting "red"
		// wants red — with only blue/cyan pulled onto the brand's steel.
		ansiBlack:   BrandBgLeast,
		ansiRed:     charmtone.Coral,
		ansiGreen:   charmtone.Guac,
		ansiYellow:  charmtone.Mustard,
		ansiBlue:    lipgloss.Color("#5B7FA6"),
		ansiMagenta: charmtone.Dolly,
		ansiCyan:    BrandAccent,
		ansiWhite:   BrandFgSubtle,

		ansiBrightBlack:   BrandBgMost,
		ansiBrightRed:     charmtone.Tuna,
		ansiBrightGreen:   charmtone.Julep,
		ansiBrightYellow:  charmtone.Zest,
		ansiBrightBlue:    BrandKeyword,
		ansiBrightMagenta: charmtone.Blush,
		ansiBrightCyan:    lipgloss.Color("#8FD4DF"),
		ansiBrightWhite:   BrandNeutralBright,
	})

	// Bang ! prompt: the accent carries it, on the bright neutral.
	s.Editor.PromptBangIconFocused = s.Editor.PromptBangIconFocused.
		Foreground(BrandNeutralBright).
		Background(BrandAccent)
	s.Editor.PromptBangDotsFocused = s.Editor.PromptBangDotsFocused.
		Foreground(BrandAccent)
	s.Editor.PromptBangDotsBlurred = s.Editor.PromptBangDotsBlurred.
		Foreground(BrandAccentDeep)

	// Shell bar/prompt.
	s.Messages.ShellBarFocused = s.Messages.ShellBarFocused.
		BorderForeground(BrandPrimary)
	s.Messages.ShellBarBlurred = s.Messages.ShellBarBlurred.
		BorderForeground(BrandBgMost)
	s.Messages.ShellPrompt = s.Messages.ShellPrompt.
		Foreground(BrandAccent)
	s.Messages.ShellPromptBlurred = s.Messages.ShellPromptBlurred.
		Foreground(BrandAccent)
	s.Files.Additions = s.Files.Additions.Foreground(BrandSuccess)

	// Check marks are green everywhere they appear — the one place the
	// scheme spends a second hue. A ✓ is a discrete semantic mark rather
	// than decoration: against muted or struck-through text a neutral check
	// reads as "also grayed out" instead of "done". Only the glyph carries
	// the color; the text around it stays as it was.
	// The "enabled" dot in resource lists (skills, MCP servers) is the same
	// semantic green, not the raw ANSI green quickStyle defaults it to —
	// that one is tuned for remapped shell output and reads as neon here.
	s.Resource.EnabledIcon = s.Resource.EnabledIcon.Foreground(BrandSuccess)

	// A finished thread on the dashboard is marked with the same green as
	// a finished todo. Left on the default neutral it was pixel-identical
	// to idle, which is the one distinction that screen exists to make.
	s.Threads.StatusDone = s.Threads.StatusDone.Foreground(BrandSuccess)

	s.ToolCallSuccess = s.ToolCallSuccess.Foreground(BrandSuccess)
	s.Tool.IconSuccess = s.Tool.IconSuccess.Foreground(BrandSuccess)
	s.Tool.JobIconSuccess = s.Tool.JobIconSuccess.Foreground(BrandSuccess)
	s.Tool.TodoCompletedIcon = s.Tool.TodoCompletedIcon.Foreground(BrandSuccess)
	s.Status.SuccessIndicator = s.Status.SuccessIndicator.Foreground(BrandSuccess)

	return s
}
