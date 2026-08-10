// Command genicon regenerates Braid's embedded notification icon. Run via
// `go generate ./internal/ui/notification/...`; see the go:generate
// directive in internal/ui/notification/icon.go. Writes deterministic
// output, so a clean regeneration should never produce a diff.
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/rave-soft/braid/internal/ui/notification/genicon"
)

func main() {
	// go:generate runs with the working directory set to the package
	// containing the directive (internal/ui/notification), so this path is
	// relative to there.
	out := filepath.Join("assets", "braid.png")
	if err := os.WriteFile(out, genicon.Render(), 0o644); err != nil {
		log.Fatalf("genicon: write %s: %v", out, err)
	}
}
