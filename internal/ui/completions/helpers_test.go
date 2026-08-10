package completions

import "charm.land/lipgloss/v2"

// testStyles returns a plain PopupStyles for tests that don't care about
// actual colors/borders — just the popup's behavior.
func testStyles() PopupStyles {
	s := lipgloss.NewStyle()
	return PopupStyles{
		Normal:         s,
		Focused:        s,
		Match:          s,
		Muted:          s,
		Border:         s,
		ScrollbarThumb: s,
		ScrollbarTrack: s,
	}
}
