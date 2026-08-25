package tools

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rave-soft/sennit/internal/log"
)

func getRg() string {
	return lookupRipgrep(exec.LookPath)
}

func lookupRipgrep(lookup func(string) (string, error)) string {
	path, err := lookup("rg")
	if err != nil {
		if log.Initialized() {
			slog.Warn("Ripgrep (rg) not found in $PATH. Some grep features might be limited or slower.")
		}
		return ""
	}
	return path
}

// HasRipgrep reports whether the rg binary is available on this system.
func HasRipgrep() bool {
	return getRg() != ""
}

func getRgCmd(ctx context.Context, globPattern string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	// Note: we intentionally do not pass -L (follow symlinks). Following
	// symlinks lets rg escape the search root (into module caches, the nix
	// store, $HOME, etc.) and chase cycles, which pins all cores and can
	// hang. This keeps glob scoped to the tree it was pointed at, matching
	// the grep search command.
	args := []string{"--files", "--null"}
	if globPattern != "" {
		if !filepath.IsAbs(globPattern) && !strings.HasPrefix(globPattern, "/") {
			globPattern = "/" + globPattern
		}
		args = append(args, "--glob", globPattern)
	}
	return exec.CommandContext(ctx, name, args...)
}

func getRgSearchCmd(ctx context.Context, pattern, path, include string, caseInsensitive bool) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	return newRgSearchCmd(ctx, name, pattern, path, include, caseInsensitive)
}

func newRgSearchCmd(ctx context.Context, name, pattern, path, include string, caseInsensitive bool) *exec.Cmd {
	// Use -n to show line numbers, -0 for null separation to handle Windows paths
	args := []string{"--json", "-H", "-n", "-0"}
	if caseInsensitive {
		args = append(args, "-i")
	}
	args = append(args, pattern)
	if include != "" {
		args = append(args, "--glob", include)
	}
	args = append(args, path)

	return exec.CommandContext(ctx, name, args...)
}
