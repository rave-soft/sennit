package styles

import (
	"charm.land/bubbles/v2/filepicker"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/ui/diffview"
)

// quickStyleDiff fills in Diff (the diffview line styles) and FilePicker.
func quickStyleDiff(s *Styles, o quickStyleOpts, base, _, _ lipgloss.Style) {
	s.Diff = diffview.Style{
		DividerLine: diffview.LineStyle{
			LineNumber: lipgloss.NewStyle().
				Foreground(o.fgSubtle).
				Background(o.bgLeastVisible),
			Code: lipgloss.NewStyle().
				Foreground(o.fgSubtle).
				Background(o.bgLeastVisible),
		},
		MissingLine: diffview.LineStyle{
			LineNumber: lipgloss.NewStyle().
				Background(o.bgLeastVisible),
			Code: lipgloss.NewStyle().
				Background(o.bgLeastVisible),
		},
		EqualLine: diffview.LineStyle{
			LineNumber: lipgloss.NewStyle().
				Foreground(o.fgMoreSubtle).
				Background(o.bgBase),
			Code: lipgloss.NewStyle().
				Foreground(o.fgMoreSubtle).
				Background(o.bgBase),
		},
		InsertLine: diffview.LineStyle{
			LineNumber: lipgloss.NewStyle().
				Foreground(o.diffInsertFg).
				Background(o.diffInsertBgDim),
			Symbol: lipgloss.NewStyle().
				Foreground(o.diffInsertFg).
				Background(o.diffInsertBg),
			Code: lipgloss.NewStyle().
				Background(o.diffInsertBg),
		},
		DeleteLine: diffview.LineStyle{
			LineNumber: lipgloss.NewStyle().
				Foreground(o.diffDeleteFg).
				Background(o.diffDeleteBgDim),
			Symbol: lipgloss.NewStyle().
				Foreground(o.diffDeleteFg).
				Background(o.diffDeleteBg),
			Code: lipgloss.NewStyle().
				Background(o.diffDeleteBg),
		},
		Filename: diffview.LineStyle{
			LineNumber: lipgloss.NewStyle().
				Foreground(o.fgSubtle).
				Background(o.bgLeastVisible),
			Code: lipgloss.NewStyle().
				Foreground(o.fgSubtle).
				Background(o.bgLeastVisible),
		},
	}

	s.FilePicker = filepicker.Styles{
		DisabledCursor:   base.Foreground(o.fgMoreSubtle),
		Cursor:           base.Foreground(o.fgBase),
		Symlink:          base.Foreground(o.fgMostSubtle),
		Directory:        base.Foreground(o.primary),
		File:             base.Foreground(o.fgBase),
		DisabledFile:     base.Foreground(o.fgMoreSubtle),
		DisabledSelected: base.Background(o.bgMostVisible).Foreground(o.fgMoreSubtle),
		Permission:       base.Foreground(o.fgMoreSubtle),
		Selected:         base.Background(o.primary).Foreground(o.fgBase),
		FileSize:         base.Foreground(o.fgMoreSubtle),
		EmptyDirectory:   base.Foreground(o.fgMoreSubtle).PaddingLeft(2).SetString("Empty directory"),
	}
}
