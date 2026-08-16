package styles

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Palette is the full set of colors one theme is built from. Every value the
// Styles graph needs lives here, so a new theme is a new Palette literal
// rather than a fork of the style-building code: quickStyle and the
// adjustments in themeFromPalette read the palette and nothing else.
//
// The schemes are deliberately restrained. Everyday chrome (tool names,
// status icons, hooks, todos) renders in the neutrals and the single accent,
// so a screen reads as monochrome with one accent. Saturated colors are
// reserved for rare, genuinely semantic states: errors, warnings,
// destructive dialogs, and the green that marks something done.
type Palette struct {
	// ID is the stable identifier persisted in options.tui.theme.
	ID string
	// Name and Description label the theme in the picker.
	Name        string
	Description string

	// Brand.
	Primary   color.Color
	Secondary color.Color
	Accent    color.Color
	Keyword   color.Color

	// Foreground ramp, brightest first.
	Fg           color.Color
	FgSubtle     color.Color
	FgMoreSubtle color.Color
	FgMostSubtle color.Color
	FgMarkdown   color.Color

	// Background ramp and the chrome derived from it.
	Bg            color.Color
	BgMarkdown    color.Color
	BgHover       color.Color
	BgLeast       color.Color
	BgLess        color.Color
	BgMost        color.Color
	Separator     color.Color
	OnPrimary     color.Color
	AccentDeep    color.Color
	NeutralBright color.Color

	// Syntax highlighting (markdown code blocks, chroma). Code is the one
	// place that genuinely needs several distinguishable hues, so this is
	// not reduced to the single accent — but it is built from the same base,
	// usually with a warm exception or two so literals stay readable.
	SyntaxKeyword   color.Color
	SyntaxType      color.Color
	SyntaxBuiltin   color.Color
	SyntaxTag       color.Color
	SyntaxAttribute color.Color
	SyntaxOperator  color.Color
	SyntaxClass     color.Color
	SyntaxString    color.Color
	SyntaxDecorator color.Color
	SyntaxPreproc   color.Color
	SyntaxLink      color.Color

	// Semantic. The only saturated colors in a scheme.
	Success     color.Color
	Destructive color.Color
	Error       color.Color
	Warning     color.Color
	WarningSoft color.Color
	Attention   color.Color
}

// PaletteSteelTeal is Sennit's default scheme: cool steel neutrals with a
// single muted teal accent. The most engineering-room of the four and the
// one the rest are calibrated against.
var PaletteSteelTeal = Palette{
	ID:          "steel-teal",
	Name:        "Steel & Teal",
	Description: "cool steel neutrals, one muted teal accent",

	Primary:   lipgloss.Color("#4A8FA8"), // steel teal
	Secondary: lipgloss.Color("#6ECBD6"), // aqua; the far end of the working gradient
	Accent:    lipgloss.Color("#5FB3C4"), // the one accent everyday chrome may use
	Keyword:   lipgloss.Color("#7FA8C9"), // slate blue

	// Cool (blue-tinted) rather than the stock charmtone neutrals, which
	// lean warm/purple.
	Fg:           lipgloss.Color("#E4EBF3"),
	FgSubtle:     lipgloss.Color("#AEBDCC"),
	FgMoreSubtle: lipgloss.Color("#7E8B99"),
	FgMostSubtle: lipgloss.Color("#57626E"),
	FgMarkdown:   lipgloss.Color("#C7D2DE"),

	Bg:            lipgloss.Color("#0F1216"),
	BgMarkdown:    lipgloss.Color("#1A1F25"),
	BgHover:       lipgloss.Color("#222831"),
	BgLeast:       lipgloss.Color("#151A20"),
	BgLess:        lipgloss.Color("#1E242B"),
	BgMost:        lipgloss.Color("#2C343D"),
	Separator:     lipgloss.Color("#232A32"),
	OnPrimary:     lipgloss.Color("#08131A"),
	AccentDeep:    lipgloss.Color("#3B6675"),
	NeutralBright: lipgloss.Color("#F2F6FA"),

	SyntaxKeyword:   lipgloss.Color("#7FA8C9"),
	SyntaxType:      lipgloss.Color("#6ECBD6"),
	SyntaxBuiltin:   lipgloss.Color("#8FD4DF"),
	SyntaxTag:       lipgloss.Color("#86A5B8"),
	SyntaxAttribute: lipgloss.Color("#5FB3C4"),
	SyntaxOperator:  lipgloss.Color("#9AAAB8"),
	SyntaxClass:     lipgloss.Color("#F2F6FA"),
	SyntaxString:    lipgloss.Color("#8FB593"),
	SyntaxDecorator: lipgloss.Color("#C9B25E"),
	SyntaxPreproc:   lipgloss.Color("#C98F5E"),
	SyntaxLink:      lipgloss.Color("#7FA8C9"),

	Success:     lipgloss.Color("#6E9E7C"), // sage green: "done", check marks
	Destructive: lipgloss.Color("#D9705F"),
	Error:       lipgloss.Color("#C4483A"),
	Warning:     lipgloss.Color("#C99A3E"),
	WarningSoft: lipgloss.Color("#D9A857"),
	Attention:   lipgloss.Color("#D98E4E"),
}

// PaletteGraphiteAmber is the warm scheme: neutral graphite with an
// amber-bronze accent. Softer and more expensive-looking than the others.
// Because the accent sits where a warning normally would, warnings here are
// pushed into red-orange so the two never read as the same signal.
var PaletteGraphiteAmber = Palette{
	ID:          "graphite-amber",
	Name:        "Graphite & Amber",
	Description: "warm graphite neutrals, amber-bronze accent",

	Primary:   lipgloss.Color("#C08A3E"),
	Secondary: lipgloss.Color("#E0BE7E"),
	Accent:    lipgloss.Color("#D9A857"),
	Keyword:   lipgloss.Color("#B8895F"),

	Fg:           lipgloss.Color("#EDE8E0"),
	FgSubtle:     lipgloss.Color("#CBC3B8"),
	FgMoreSubtle: lipgloss.Color("#A2998C"),
	FgMostSubtle: lipgloss.Color("#6F675C"),
	FgMarkdown:   lipgloss.Color("#DCD5C9"),

	Bg:            lipgloss.Color("#14120F"),
	BgMarkdown:    lipgloss.Color("#1F1C17"),
	BgHover:       lipgloss.Color("#2A251E"),
	BgLeast:       lipgloss.Color("#1A1712"),
	BgLess:        lipgloss.Color("#23201A"),
	BgMost:        lipgloss.Color("#332E27"),
	Separator:     lipgloss.Color("#2A2620"),
	OnPrimary:     lipgloss.Color("#1A1206"),
	AccentDeep:    lipgloss.Color("#8A6430"),
	NeutralBright: lipgloss.Color("#FAF6F0"),

	SyntaxKeyword:   lipgloss.Color("#B8895F"),
	SyntaxType:      lipgloss.Color("#D9A857"),
	SyntaxBuiltin:   lipgloss.Color("#E0BE7E"),
	SyntaxTag:       lipgloss.Color("#A8977F"),
	SyntaxAttribute: lipgloss.Color("#C08A3E"),
	SyntaxOperator:  lipgloss.Color("#A79C8C"),
	SyntaxClass:     lipgloss.Color("#FAF6F0"),
	SyntaxString:    lipgloss.Color("#7FA36B"),
	SyntaxDecorator: lipgloss.Color("#C9A227"),
	SyntaxPreproc:   lipgloss.Color("#C4664F"),
	SyntaxLink:      lipgloss.Color("#B8895F"),

	Success:     lipgloss.Color("#7FA36B"),
	Destructive: lipgloss.Color("#C4664F"),
	Error:       lipgloss.Color("#B4453A"),
	// Amber is the accent here, so the warning ramp moves to red-orange.
	Warning:     lipgloss.Color("#D2603C"),
	WarningSoft: lipgloss.Color("#E0784E"),
	Attention:   lipgloss.Color("#D2603C"),
}

// PaletteInkSage is the classic terminal scheme: deep blue-black ground with
// a muted sage green. Its one tradeoff is that the accent and "success" come
// from the same family, so done-ness is carried by the glyph shape (✓) as
// much as by the color.
var PaletteInkSage = Palette{
	ID:          "ink-sage",
	Name:        "Ink & Sage",
	Description: "deep blue-black ground, muted sage accent",

	Primary:   lipgloss.Color("#6E9E7C"),
	Secondary: lipgloss.Color("#A9D4B6"),
	Accent:    lipgloss.Color("#8FBF9F"),
	Keyword:   lipgloss.Color("#86A5B8"),

	Fg:           lipgloss.Color("#E2E8E4"),
	FgSubtle:     lipgloss.Color("#B9C4BE"),
	FgMoreSubtle: lipgloss.Color("#8A9691"),
	FgMostSubtle: lipgloss.Color("#5E6B66"),
	FgMarkdown:   lipgloss.Color("#CBD5CF"),

	Bg:            lipgloss.Color("#0D1114"),
	BgMarkdown:    lipgloss.Color("#171D21"),
	BgHover:       lipgloss.Color("#212A2D"),
	BgLeast:       lipgloss.Color("#131A1D"),
	BgLess:        lipgloss.Color("#1B2327"),
	BgMost:        lipgloss.Color("#2A3438"),
	Separator:     lipgloss.Color("#212A2D"),
	OnPrimary:     lipgloss.Color("#07120C"),
	AccentDeep:    lipgloss.Color("#4C7259"),
	NeutralBright: lipgloss.Color("#F1F6F3"),

	SyntaxKeyword:   lipgloss.Color("#86A5B8"),
	SyntaxType:      lipgloss.Color("#8FBF9F"),
	SyntaxBuiltin:   lipgloss.Color("#A9D4B6"),
	SyntaxTag:       lipgloss.Color("#7E9AA8"),
	SyntaxAttribute: lipgloss.Color("#6E9E7C"),
	SyntaxOperator:  lipgloss.Color("#94A39C"),
	SyntaxClass:     lipgloss.Color("#F1F6F3"),
	SyntaxString:    lipgloss.Color("#A9C08A"),
	SyntaxDecorator: lipgloss.Color("#C9B25E"),
	SyntaxPreproc:   lipgloss.Color("#BE7160"),
	SyntaxLink:      lipgloss.Color("#86A5B8"),

	Success:     lipgloss.Color("#8FBF9F"),
	Destructive: lipgloss.Color("#BE7160"),
	Error:       lipgloss.Color("#B4544A"),
	Warning:     lipgloss.Color("#C9A65E"),
	WarningSoft: lipgloss.Color("#D6B673"),
	Attention:   lipgloss.Color("#C98F5E"),
}

// PaletteMonoSteel is the most restrained scheme: all chrome is gray and the
// only color is a steel blue, with everything else left to semantics. The
// least branded of the four, by design.
var PaletteMonoSteel = Palette{
	ID:          "mono-steel",
	Name:        "Mono & Steel",
	Description: "gray chrome, a single steel blue",

	Primary:   lipgloss.Color("#5B7FA6"),
	Secondary: lipgloss.Color("#9FB6CE"),
	Accent:    lipgloss.Color("#7C9CBF"),
	Keyword:   lipgloss.Color("#A8B4C0"),

	Fg:           lipgloss.Color("#E4EBF3"),
	FgSubtle:     lipgloss.Color("#B4BDC7"),
	FgMoreSubtle: lipgloss.Color("#868F9B"),
	FgMostSubtle: lipgloss.Color("#5C6570"),
	FgMarkdown:   lipgloss.Color("#C6CDD6"),

	Bg:            lipgloss.Color("#101215"),
	BgMarkdown:    lipgloss.Color("#191D22"),
	BgHover:       lipgloss.Color("#232830"),
	BgLeast:       lipgloss.Color("#15181D"),
	BgLess:        lipgloss.Color("#1D2229"),
	BgMost:        lipgloss.Color("#2C323A"),
	Separator:     lipgloss.Color("#232830"),
	OnPrimary:     lipgloss.Color("#080D12"),
	AccentDeep:    lipgloss.Color("#405B78"),
	NeutralBright: lipgloss.Color("#F3F6FA"),

	SyntaxKeyword:   lipgloss.Color("#A8B4C0"),
	SyntaxType:      lipgloss.Color("#7C9CBF"),
	SyntaxBuiltin:   lipgloss.Color("#9FB6CE"),
	SyntaxTag:       lipgloss.Color("#8D97A3"),
	SyntaxAttribute: lipgloss.Color("#5B7FA6"),
	SyntaxOperator:  lipgloss.Color("#99A2AD"),
	SyntaxClass:     lipgloss.Color("#F3F6FA"),
	SyntaxString:    lipgloss.Color("#8FB593"),
	SyntaxDecorator: lipgloss.Color("#C9B25E"),
	SyntaxPreproc:   lipgloss.Color("#C98F5E"),
	SyntaxLink:      lipgloss.Color("#7C9CBF"),

	Success:     lipgloss.Color("#6E9E7C"),
	Destructive: lipgloss.Color("#D9705F"),
	Error:       lipgloss.Color("#C4483A"),
	Warning:     lipgloss.Color("#C99A3E"),
	WarningSoft: lipgloss.Color("#D9A857"),
	Attention:   lipgloss.Color("#D98E4E"),
}

// DefaultThemeID names the palette used when nothing is configured.
const DefaultThemeID = "steel-teal"

// palettes is the ordered registry backing the theme picker. Order is the
// display order; it is not alphabetical, it runs from the default outward.
var palettes = []Palette{
	PaletteSteelTeal,
	PaletteGraphiteAmber,
	PaletteInkSage,
	PaletteMonoSteel,
}

// Palettes returns every selectable palette in display order.
func Palettes() []Palette {
	out := make([]Palette, len(palettes))
	copy(out, palettes)
	return out
}

// PaletteByID returns the palette with the given ID. An unknown or empty ID
// resolves to the default, so a hand-edited config with a stale theme name
// degrades to Sennit's own scheme instead of failing to start.
func PaletteByID(id string) Palette {
	for _, p := range palettes {
		if p.ID == id {
			return p
		}
	}
	return PaletteSteelTeal
}

// IsKnownPaletteID reports whether id names a real palette. Callers that
// need to reject a bad value (config validation, the theme command) use this
// rather than comparing PaletteByID's result.
func IsKnownPaletteID(id string) bool {
	for _, p := range palettes {
		if p.ID == id {
			return true
		}
	}
	return false
}
