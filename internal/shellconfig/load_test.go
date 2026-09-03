package shellconfig

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/version"
	"github.com/stretchr/testify/require"
)

// TestLoadShellConfig_Provider verifies that the provider builtin produces
// correct JSON for a basic provider definition.
func TestLoadShellConfig_Provider(t *testing.T) {
	dir := t.TempDir()
	script := `provider add openai --api-key "$OPENAI_API_KEY" --base-url "https://api.openai.com/v1"`
	path := filepath.Join(dir, "sennitrc")

	t.Setenv("OPENAI_API_KEY", "test-key-123")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)
	require.NotNil(t, jsonBytes)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers, ok := result["providers"].(map[string]any)
	require.True(t, ok)
	openai, ok := providers["openai"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "test-key-123", openai["api_key"])
	require.Equal(t, "https://api.openai.com/v1", openai["base_url"])
}

// TestLoadShellConfig_FlagBoolCaseInsensitive verifies that flag booleans
// accept mixed-case values like TRUE/False.
func TestLoadShellConfig_FlagBoolCaseInsensitive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `provider add openai --api-key key --disable TRUE`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	openai := result["providers"].(map[string]any)["openai"].(map[string]any)
	require.Equal(t, true, openai["disable"])
}

// TestLoadShellConfig_MultipleProviders verifies that multiple provider calls
// each produce separate entries.
func TestLoadShellConfig_MultipleProviders(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `provider add openai --api-key "key1"
provider add anthropic --api-key "key2"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers := result["providers"].(map[string]any)
	require.Len(t, providers, 2)
	require.Equal(t, "key1", providers["openai"].(map[string]any)["api_key"])
	require.Equal(t, "key2", providers["anthropic"].(map[string]any)["api_key"])
}

// TestLoadShellConfig_Model verifies the model builtin.
func TestLoadShellConfig_Model(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `model openai/gpt-4o --think`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	model := result["model"].(map[string]any)
	require.Equal(t, "openai", model["provider"])
	require.Equal(t, "gpt-4o", model["model"])
	require.Equal(t, true, model["think"])
}

// TestLoadShellConfig_MCP verifies the mcp builtin with stdio and http types.
func TestLoadShellConfig_MCP(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `mcp add github --type stdio --command npx --args "-y" --args "@modelcontextprotocol/server-github" --env GITHUB_TOKEN "ghp_xxx"
mcp add local-server --type http --url "http://localhost:3000/mcp" --header "Authorization" "Bearer token"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	mcps := result["mcp"].(map[string]any)

	github := mcps["github"].(map[string]any)
	require.Equal(t, "stdio", github["type"])
	require.Equal(t, "npx", github["command"])
	args := github["args"].([]any)
	require.Len(t, args, 2)
	require.Equal(t, "-y", args[0])
	require.Equal(t, "@modelcontextprotocol/server-github", args[1])
	env := github["env"].(map[string]any)
	require.Equal(t, "ghp_xxx", env["GITHUB_TOKEN"])

	local := mcps["local-server"].(map[string]any)
	require.Equal(t, "http", local["type"])
	require.Equal(t, "http://localhost:3000/mcp", local["url"])
	headers := local["headers"].(map[string]any)
	require.Equal(t, "Bearer token", headers["Authorization"])
}

// TestLoadShellConfig_LSP verifies the lsp builtin.
func TestLoadShellConfig_LSP(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `lsp add gopls --command gopls --filetypes go --filetypes mod --root-markers go.mod --timeout 60`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	lsps := result["lsp"].(map[string]any)
	gopls := lsps["gopls"].(map[string]any)
	require.Equal(t, "gopls", gopls["command"])
	filetypes := gopls["filetypes"].([]any)
	require.Len(t, filetypes, 2)
	require.Equal(t, "go", filetypes[0])
	require.Equal(t, "mod", filetypes[1])
	markers := gopls["root_markers"].([]any)
	require.Len(t, markers, 1)
	require.Equal(t, "go.mod", markers[0])
	require.EqualValues(t, 60, gopls["timeout"])
}

// TestLoadShellConfig_Permissions verifies the permissions builtin.
func TestLoadShellConfig_Permissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `permissions allow bash view`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	perms := result["permissions"].(map[string]any)
	tools := perms["allowed_tools"].([]any)
	require.Len(t, tools, 2)
	require.Equal(t, "bash", tools[0])
	require.Equal(t, "view", tools[1])
}

// TestLoadShellConfig_PermissionsDeny verifies that `permissions deny` writes
// to options.disabled_tools (not permissions.disabled_tools). This
// cross-section write is load-bearing: deny wins over allow because
// disabled_tools removes a tool from the agent entirely. Pin the destination
// so a rename or relocation of disabled_tools can't silently break it.
func TestLoadShellConfig_PermissionsDeny(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `permissions deny bash fetch`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	disabled := opts["disabled_tools"].([]any)
	require.Equal(t, []any{"bash", "fetch"}, disabled)
	require.NotContains(t, result, "permissions",
		"deny must not create a permissions section")
}

// TestLoadShellConfig_PermissionsBypass verifies that `permissions bypass
// on|off` writes the bool permissions.bypass flag.
func TestLoadShellConfig_PermissionsBypass(t *testing.T) {
	t.Parallel()

	t.Run("on", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		script := `permissions bypass on`
		path := filepath.Join(dir, "sennitrc")

		jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.NoError(t, err)

		var result map[string]any
		require.NoError(t, json.Unmarshal(jsonBytes, &result))

		perms := result["permissions"].(map[string]any)
		require.Equal(t, true, perms["bypass"])
	})

	t.Run("off", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		script := `permissions bypass off`
		path := filepath.Join(dir, "sennitrc")

		jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.NoError(t, err)

		var result map[string]any
		require.NoError(t, json.Unmarshal(jsonBytes, &result))

		perms := result["permissions"].(map[string]any)
		require.Equal(t, false, perms["bypass"])
	})

	t.Run("invalid argument", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		script := `permissions bypass maybe`
		path := filepath.Join(dir, "sennitrc")

		_, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.Error(t, err)
	})

	t.Run("missing argument", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		script := `permissions bypass`
		path := filepath.Join(dir, "sennitrc")

		_, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.Error(t, err)
	})
}

// TestLoadShellConfig_Hook verifies the hook builtin.
func TestLoadShellConfig_Hook(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `hook add PreToolUse --command "echo running" --matcher "bash" --timeout 10 --name "my-hook"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	hooks := result["hooks"].(map[string]any)
	preToolUse := hooks["PreToolUse"].([]any)
	require.Len(t, preToolUse, 1)
	hook := preToolUse[0].(map[string]any)
	require.Equal(t, "echo running", hook["command"])
	require.Equal(t, "bash", hook["matcher"])
	require.EqualValues(t, 10, hook["timeout"])
	require.Equal(t, "my-hook", hook["name"])
}

// TestLoadShellConfig_Option verifies the option builtin.
func TestLoadShellConfig_Option(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option data-directory .sennit
option metrics false
option debug`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, ".sennit", opts["data_directory"])
	require.Equal(t, true, opts["disable_metrics"])
	require.Equal(t, true, opts["debug"])
}

// TestLoadShellConfig_SourceInclude verifies that source works for includes.
func TestLoadShellConfig_SourceInclude(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create an included file with a provider definition.
	includeContent := `provider add openai --api-key "included-key"`
	includePath := filepath.Join(dir, "shared.sh")
	require.NoError(t, os.WriteFile(includePath, []byte(includeContent), 0o644))

	// Create the main script that sources the include. Use forward
	// slashes so the path survives the bash interpreter on Windows,
	// where backslashes would be treated as escape characters.
	script := `source ` + filepath.ToSlash(includePath) + `
provider add anthropic --api-key "main-key"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers := result["providers"].(map[string]any)
	require.Len(t, providers, 2)
	require.Equal(t, "included-key", providers["openai"].(map[string]any)["api_key"])
	require.Equal(t, "main-key", providers["anthropic"].(map[string]any)["api_key"])
}

// TestLoadShellConfig_Conditionals verifies that bash conditionals work.
func TestLoadShellConfig_Conditionals(t *testing.T) {
	dir := t.TempDir()
	script := `if [[ "$USE_ANTHROPIC" == "1" ]]; then
  provider add anthropic --api-key "ant-key"
else
  provider add openai --api-key "oai-key"
fi`
	path := filepath.Join(dir, "sennitrc")

	t.Setenv("USE_ANTHROPIC", "1")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers := result["providers"].(map[string]any)
	require.Len(t, providers, 1)
	require.Contains(t, providers, "anthropic")
}

// TestLoadShellConfig_SennitVersionEnv verifies that SENNIT_VERSION is exposed
// to the script so it can feature-detect the running Sennit version.
func TestLoadShellConfig_SennitVersionEnv(t *testing.T) {
	dir := t.TempDir()
	script := `provider add openai --api-key "$SENNIT_VERSION"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers := result["providers"].(map[string]any)
	openai := providers["openai"].(map[string]any)
	require.Equal(t, version.Version, openai["api_key"])
}

// TestLoadShellConfig_CommandSubstitution verifies that $(...) works in config values.
func TestLoadShellConfig_CommandSubstitution(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `provider add openai --api-key "$(echo dynamic-key)"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers := result["providers"].(map[string]any)
	openai := providers["openai"].(map[string]any)
	require.Equal(t, "dynamic-key", openai["api_key"])
}

// TestLoadShellConfig_EnvVarExpansion verifies that $VAR expansion works.
func TestLoadShellConfig_EnvVarExpansion(t *testing.T) {
	dir := t.TempDir()
	script := `provider add openai --api-key "$MY_API_KEY"`
	path := filepath.Join(dir, "sennitrc")

	t.Setenv("MY_API_KEY", "env-key-456")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers := result["providers"].(map[string]any)
	openai := providers["openai"].(map[string]any)
	require.Equal(t, "env-key-456", openai["api_key"])
}

// TestLoadShellConfig_UnknownFlag verifies error handling for unknown flags.
func TestLoadShellConfig_UnknownFlag(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `provider add openai --bogus-flag "value"`
	path := filepath.Join(dir, "sennitrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
}

// TestLoadShellConfig_MissingRequiredArgs verifies error handling for missing args.
func TestLoadShellConfig_MissingRequiredArgs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `provider`
	path := filepath.Join(dir, "sennitrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
}

// TestLoadShellConfig_NoBuiltins verifies that a script with no config builtins
// produces no output.
func TestLoadShellConfig_NoBuiltins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `echo "just a normal script"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)
	require.Nil(t, jsonBytes)
}

func TestLoadShellConfig_ProviderJSONFlagsRequireObjects(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`provider add custom --extra-body '[]'`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects a JSON object")
}

// TestLoadShellConfig_ExtraHeader verifies the --extra-header flag.
func TestLoadShellConfig_ExtraHeader(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `provider add custom --api-key "key" --extra-header "X-Custom" "value123"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers := result["providers"].(map[string]any)
	custom := providers["custom"].(map[string]any)
	headers := custom["extra_headers"].(map[string]any)
	require.Equal(t, "value123", headers["X-Custom"])
}

// TestLoadShellConfig_FullConfig verifies a complete config with all builtins.
func TestLoadShellConfig_FullConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "oai-key")
	t.Setenv("ANTHROPIC_API_KEY", "ant-key")

	script := `#!/usr/bin/env bash

# Providers
provider add openai --api-key "$OPENAI_API_KEY" --base-url "https://api.openai.com/v1"
provider add anthropic --api-key "$ANTHROPIC_API_KEY"
provider add my-llm --type openai --api-key "ollama" --base-url "http://localhost:11434/v1"

# Models
model openai/gpt-4o --think

# MCP
mcp add github --type stdio --command npx --args "-y" --args "@modelcontextprotocol/server-github"

# LSP
lsp add gopls --command gopls --filetypes go --root-markers go.mod

# Permissions
permissions allow bash view

# Hooks
hook add PreToolUse --command "echo running" --matcher "bash" --timeout 10

# Options
option data-directory .sennit
option metrics false`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)
	require.NotNil(t, jsonBytes)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	// Verify providers
	providers := result["providers"].(map[string]any)
	require.Len(t, providers, 3)
	require.Equal(t, "oai-key", providers["openai"].(map[string]any)["api_key"])
	require.Equal(t, "ant-key", providers["anthropic"].(map[string]any)["api_key"])
	myLLM := providers["my-llm"].(map[string]any)
	require.Equal(t, "ollama", myLLM["api_key"])
	require.Equal(t, "http://localhost:11434/v1", myLLM["base_url"])

	// Verify models
	model := result["model"].(map[string]any)
	require.Equal(t, "openai", model["provider"])
	require.Equal(t, "gpt-4o", model["model"])
	require.Equal(t, true, model["think"])

	// Verify MCP
	mcps := result["mcp"].(map[string]any)
	github := mcps["github"].(map[string]any)
	require.Equal(t, "npx", github["command"])

	// Verify LSP
	lsps := result["lsp"].(map[string]any)
	require.Contains(t, lsps, "gopls")

	// Verify permissions
	perms := result["permissions"].(map[string]any)
	require.Contains(t, perms, "allowed_tools")

	// Verify hooks
	hooks := result["hooks"].(map[string]any)
	require.Contains(t, hooks, "PreToolUse")

	// Verify options
	opts := result["options"].(map[string]any)
	require.Equal(t, ".sennit", opts["data_directory"])
	require.Equal(t, true, opts["disable_metrics"])
}

// TestConfigBuilder_NoBuilderInContext verifies that config builtins do not
// intercept their name when no ConfigBuilder is on the context (normal
// bash tool execution): "provider" falls through to the real exec path
// instead of being swallowed as a silent success, so with no "provider"
// program on PATH the command fails the ordinary "command not found" way.
// Shadowing it — reporting success without running anything — is exactly
// the bug this pins against; see TestConfigBuiltins_FallThroughWithoutConfigBuilder
// in register_test.go for the fall-through-to-a-real-program case.
func TestConfigBuilder_NoBuilderInContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := shell.Run(t.Context(), shell.RunOptions{
		Command: `provider add openai --api-key "test"`,
		Cwd:     dir,
		Env:     os.Environ(),
	})
	require.Error(t, err, "provider must not silently succeed without a ConfigBuilder on the context")
}

// TestLoadShellConfig_TrailingNonZeroStatusKeepsConfig verifies that a
// script whose last command exits non-zero still yields the config it
// built rather than discarding it. This is the ordinary rc idiom of a
// trailing probe, e.g. `command -v foo >/dev/null && lsp add foo ...`:
// bash does not refuse to start because .bashrc's last line returned
// non-zero, and Sennit must not either.
func TestLoadShellConfig_TrailingNonZeroStatusKeepsConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `provider add openai --api-key k
command -v definitely-not-installed-xyz >/dev/null && provider add other --api-key k2`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err, "a non-zero trailing script status must not fail the load")
	require.NotNil(t, jsonBytes)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))
	providers := result["providers"].(map[string]any)
	require.Contains(t, providers, "openai", "config built before the failing probe must survive")
	require.NotContains(t, providers, "other", "the probe itself must not have run")
}

// TestLoadShellConfig_BuiltinErrorStillFails verifies that a builtin
// failure (not just a script's own exit status) still fails the load, even
// when it isn't the script's last line. Builtin errors surface as their
// own error value, distinct from interp.ExitStatus, so they must not be
// swallowed by the trailing-status tolerance above.
func TestLoadShellConfig_BuiltinErrorStillFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option --bogus foo
true`
	path := filepath.Join(dir, "sennitrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err, "a builtin error must fail the load even when a later line succeeds")
}

// TestLoadShellConfig_RespectsContextCancellation verifies that a hanging
// sennitrc cannot block config loading indefinitely. Config loads run on the
// startup and reload critical paths while the config store's write lock is
// held, so a runaway script (a busy loop, a hung command substitution) must
// be interruptible via the context rather than wedging the whole store. The
// test bounds its own wait so a regression can't hang CI.
func TestLoadShellConfig_RespectsContextCancellation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	script := `while true; do :; done`

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		_, err := LoadShellConfig(ctx, path, []byte(script))
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a cancelled sennitrc must fail, not succeed")
		require.True(t, shell.IsInterrupt(err),
			"expected an interrupt/cancellation error, got: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("LoadShellConfig did not return after context cancellation")
	}
}

// TestLoadShellConfig_StripsHerdrEnv pins the same guarantee the bash tool
// and hooks give: a sennitrc script must not see the process's HERDR_*
// vars, or a nested sennit it starts could attach to the parent pane's
// agent authority.
func TestLoadShellConfig_StripsHerdrEnv(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	t.Setenv("HERDR_PANE_ID", "wA:p1")

	dir := t.TempDir()
	script := `provider add openai --api-key "[$HERDR_SOCKET_PATH][$HERDR_PANE_ID]"`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	providers := result["providers"].(map[string]any)
	openai := providers["openai"].(map[string]any)
	require.Equal(t, "[][]", openai["api_key"], "herdr vars leaked into the sennitrc script env")
}
