package shellconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/version"
	"mvdan.cc/sh/v3/interp"
)

// loadTimeout bounds a single sennitrc execution. This runs on the startup
// and reload critical paths — Load and reloadFromDisk both call it before
// ever taking the config store's write lock, specifically so a slow script
// can't hold that lock — but callers there still block synchronously
// waiting on the result, so a script that hangs (a stuck command
// substitution, a stray loop) must not be able to wedge them forever. The
// interpreter honors context cancellation, so this deadline reliably
// interrupts a runaway script.
const loadTimeout = 30 * time.Second

// LoadShellConfig executes a sennitrc script and returns its config as a
// single JSON object. The script uses config builtins (provider, model, mcp,
// etc.) that mutate a ConfigBuilder in execution order; the builder is then
// marshaled to JSON, which the config loader merges with any other config
// files.
//
// The script runs with the same shell interpreter used by the bash tool and
// hooks, so source, $VAR, $(cmd), and other shell constructs all work.
//
// Execution is bounded by loadTimeout on top of any deadline already present
// on ctx, so a misbehaving script cannot block config loading indefinitely.
func LoadShellConfig(ctx context.Context, path string, src []byte) ([]byte, error) {
	slog.Info("Loading shell config", "path", path)

	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()

	builder := newConfigBuilder()
	runCtx := withConfigBuilder(ctx, builder)

	cwd := filepath.Dir(path)

	// Expose the running Sennit version so scripts can feature-detect, e.g.
	// [[ "$SENNIT_VERSION" == "devel" ]] or branch on the release.
	env := append(os.Environ(), brand.EnvPrefix+"VERSION="+version.Version)

	err := shell.Run(runCtx, shell.RunOptions{
		Command: string(src),
		Cwd:     cwd,
		Env:     env,
	})
	if err != nil {
		if shell.IsInterrupt(err) {
			slog.Error("Shell config execution timed out or was cancelled", "path", path, "error", err)
			return nil, fmt.Errorf("shell config %s: execution timed out or was cancelled: %w", path, err)
		}
		// A bare interp.ExitStatus is the exit code of the script's last
		// command, not a builtin failure: bash itself doesn't refuse to
		// start because .bashrc's last line returned non-zero, and rc
		// idioms like `command -v foo >/dev/null && lsp add foo ...` end
		// on a probe that can legitimately fail. A builtin error (bad
		// flag, unknown option, etc.) surfaces as its own error value, not
		// as interp.ExitStatus, so it still falls through and fails the
		// load below. Log the status and keep whatever config the script
		// built before it exited.
		var exitStatus interp.ExitStatus
		if errors.As(err, &exitStatus) {
			slog.Warn("Shell config exited non-zero", "path", path, "status", int(exitStatus))
		} else {
			slog.Error("Shell config execution failed", "path", path, "error", err)
			return nil, fmt.Errorf("executing shell config %s: %w", path, err)
		}
	}

	if builder.empty() {
		slog.Warn("Shell config produced no config", "path", path)
		return nil, nil
	}

	data, err := builder.JSON()
	if err != nil {
		slog.Error("Failed to marshal shell config", "path", path, "error", err)
		return nil, fmt.Errorf("marshaling shell config %s: %w", path, err)
	}

	slog.Info("Shell config loaded successfully", "path", path, "bytes", len(data))
	return data, nil
}
