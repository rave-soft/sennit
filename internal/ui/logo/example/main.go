package main

// This is an example for testing logo treatments. Do not remove.

import (
	"fmt"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/rave-soft/braid/internal/ui/logo"
	"github.com/rave-soft/braid/internal/ui/styles"
)

func main() {
	w, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not get terminal size: %s", err)
	}

	s := styles.CharmtonePantera()
	opts := logo.Opts{
		FieldColor:   s.Logo.FieldColor,
		TitleColorA:  s.Logo.TitleColorA,
		TitleColorB:  s.Logo.TitleColorB,
		CharmColor:   s.Logo.CharmColor,
		VersionColor: s.Logo.VersionColor,
		Width:        w,
		Unstable:     true,
	}

	renderCompact := func() string {
		return logo.Render(s.Logo.GradCanvas, "v1.0.0", true, opts)
	}

	renderWide := func() string {
		return logo.Render(s.Logo.GradCanvas, "v1.0.0", false, opts)
	}

	lipgloss.Println(renderCompact())

	for range 6 {
		lipgloss.Println(renderWide())
	}
}
