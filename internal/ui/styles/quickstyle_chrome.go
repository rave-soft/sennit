package styles

import (
	"charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/anim"
)

// quickStyleWorking fills in the working-indicator gradient colors used by
// spinners/shimmers (assistant "thinking", tool-call pending, CLI
// generating, startup), how much that indicator moves, and the top-level
// Background color.
func quickStyleWorking(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.Background = o.bgBase

	s.WorkingGradFromColor = o.primary
	s.WorkingGradToColor = o.secondary
	s.WorkingLabelColor = o.fgMostSubtle
	s.WorkingTimerColor = o.fgMostSubtle
	// The default motion, written out rather than left to the zero
	// value: this is where defaults live, and a reader asking what a
	// fresh Styles animates like should find the answer next to the
	// colours it animates in. A palette has no opinion on motion, so
	// every palette gets the same one; the person's own choice arrives
	// afterwards through [Styles.WithSpinner].
	s.WorkingSpinner = anim.ModeScramble
}

// quickStyleHeader fills in Header, CompactDetails, ToolCallSuccess, and
// Help — the top chrome and shared help-view styles.
func quickStyleHeader(s *Styles, o quickStyleOpts, base, muted, subtle lipgloss.Style) {
	s.Help = help.Styles{
		ShortKey:       base.Foreground(o.fgMoreSubtle),
		ShortDesc:      base.Foreground(o.fgMostSubtle),
		ShortSeparator: base.Foreground(o.separator),
		Ellipsis:       base.Foreground(o.separator),
		FullKey:        base.Foreground(o.fgMoreSubtle),
		FullDesc:       base.Foreground(o.fgMostSubtle),
		FullSeparator:  base.Foreground(o.separator),
	}

	// borders
	s.ToolCallSuccess = lipgloss.NewStyle().Foreground(o.success).SetString(ToolSuccess)

	s.Header.Vendor = base.Foreground(o.secondary)
	s.Header.Diagonals = base.Foreground(o.primary)
	s.Header.Percentage = muted
	s.Header.Keystroke = muted
	s.Header.KeystrokeTip = subtle
	s.Header.WorkingDir = muted
	s.Header.Separator = subtle
	s.Header.Wrapper = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Header.LogoGradCanvas = lipgloss.NewStyle()
	s.Header.LogoGradFromColor = o.secondary
	s.Header.LogoGradToColor = o.primary

	s.CompactDetails.Title = base
	s.CompactDetails.View = base.Padding(0, 1, 1, 1).Border(lipgloss.RoundedBorder()).BorderForeground(o.primary)
	s.CompactDetails.Version = lipgloss.NewStyle().Foreground(o.separator)
}

// quickStyleButton fills in Button.
func quickStyleButton(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.Button.Focused = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.secondary)
	s.Button.Blurred = lipgloss.NewStyle().Foreground(o.fgBase).Background(o.bgLessVisible)
	s.Button.Hovered = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.fgMostSubtle)
	s.Button.Negative = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.error)
}

// quickStyleRadio fills in Radio.
func quickStyleRadio(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.Radio.On = lipgloss.NewStyle().Foreground(o.fgSubtle).SetString(RadioOn)
	s.Radio.Off = lipgloss.NewStyle().Foreground(o.fgSubtle).SetString(RadioOff)
	s.Radio.Label = lipgloss.NewStyle().Foreground(o.fgSubtle)
}

// quickStyleTab fills in Tab, the batch-question-form tab borders. All
// borders use charple (primary). Active tab has an open bottom that merges
// with the content area; inactive tabs have a closed bottom. First tab
// gets a right-angle bottom-left corner at draw time.
func quickStyleTab(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	borderColor := uv.Style{Fg: o.primary}
	inactiveBorder := uv.RoundedBorder().Style(borderColor)
	inactiveBorder.BottomLeft = uv.Side{Content: "┴", Style: borderColor}
	inactiveBorder.BottomRight = uv.Side{Content: "┴", Style: borderColor}
	activeBorder := uv.RoundedBorder().Style(borderColor)
	activeBorder.Bottom = uv.Side{Content: " ", Style: borderColor}
	activeBorder.BottomLeft = uv.Side{Content: "┘", Style: borderColor}
	activeBorder.BottomRight = uv.Side{Content: "└", Style: borderColor}

	s.Tab.ActiveBorder = activeBorder
	s.Tab.InactiveBorder = inactiveBorder

	blurredBorderColor := uv.Style{Fg: o.fgMoreSubtle}
	inactiveBorderBlurred := uv.RoundedBorder().Style(blurredBorderColor)
	inactiveBorderBlurred.BottomLeft = uv.Side{Content: "┴", Style: blurredBorderColor}
	inactiveBorderBlurred.BottomRight = uv.Side{Content: "┴", Style: blurredBorderColor}
	activeBorderBlurred := uv.RoundedBorder().Style(blurredBorderColor)
	activeBorderBlurred.Bottom = uv.Side{Content: " ", Style: blurredBorderColor}
	activeBorderBlurred.BottomLeft = uv.Side{Content: "┘", Style: blurredBorderColor}
	activeBorderBlurred.BottomRight = uv.Side{Content: "└", Style: blurredBorderColor}
	s.Tab.ActiveBorderBlurred = activeBorderBlurred
	s.Tab.InactiveBorderBlurred = inactiveBorderBlurred

	s.Tab.ActiveStyle = uv.Style{Fg: o.fgBase}
	s.Tab.InactiveStyle = uv.Style{Fg: o.fgMoreSubtle}
}

// quickStyleLogo fills in Logo.
func quickStyleLogo(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.Logo.FieldColor = o.primary
	s.Logo.TitleColorA = o.secondary
	s.Logo.TitleColorB = o.primary
	s.Logo.VendorColor = o.secondary
	s.Logo.VersionColor = o.primary
	s.Logo.SmallVendor = lipgloss.NewStyle().Foreground(o.secondary)
	s.Logo.SmallDiagonals = lipgloss.NewStyle().Foreground(o.primary)
	s.Logo.GradCanvas = lipgloss.NewStyle()
	s.Logo.SmallGradFromColor = o.secondary
	s.Logo.SmallGradToColor = o.primary
}
