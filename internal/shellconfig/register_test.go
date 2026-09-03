package shellconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestConfigBuiltins_FallThroughWithoutConfigBuilder pins the Defect C
// fix: without a ConfigBuilder on the context (ordinary bash tool, hook,
// or bang-mode execution — anything that isn't a sennitrc load), a name
// like "mcp" must not be swallowed by the config builtin. The real
// program on PATH has to run instead, or a genuine command like
// `mcp dev server.py` silently does nothing and reports success.
func TestConfigBuiltins_FallThroughWithoutConfigBuilder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-based fake executable setup targets POSIX shells")
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\necho real mcp cli ran\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "mcp"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout strings.Builder
	err := shell.Run(t.Context(), shell.RunOptions{
		Command: "mcp",
		Cwd:     t.TempDir(),
		Env:     os.Environ(),
		Stdout:  &stdout,
	})
	require.NoError(t, err)
	require.Equal(t, "real mcp cli ran", strings.TrimSpace(stdout.String()),
		"the config builtin shadowed the real PATH program instead of falling through")
}

// TestConfigBuiltins_ActiveDuringConfigBuilder is the companion case: the
// same "mcp" name is intercepted by the config builtin while a
// ConfigBuilder is on the context, i.e. during a real sennitrc load.
func TestConfigBuiltins_ActiveDuringConfigBuilder(t *testing.T) {
	dir := t.TempDir()
	script := `mcp add local --type http --url "http://localhost:1"`
	jsonBytes, err := LoadShellConfig(t.Context(), filepath.Join(dir, "sennitrc"), []byte(script))
	require.NoError(t, err)
	require.Contains(t, string(jsonBytes), `"local"`)
}
