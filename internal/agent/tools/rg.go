package tools

import (
	"context"
	"log/slog"
	"os/exec"

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
	// -e (rather than a bare positional) keeps a pattern starting with "-"
	// (e.g. "->") from being parsed as a flag - ripgrep would otherwise
	// exit 2 with "unrecognized flag", which the tool then surfaces as an
	// opaque "error searching files: exit status 2". literal_text:true
	// does not help: escapeRegexPattern does not escape "-".
	args = append(args, "-e", pattern)
	if include != "" {
		args = append(args, "--glob", include)
	}
	args = append(args, path)

	return exec.CommandContext(ctx, name, args...)
}
