package config

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/hooks"
)

// The hook validation and event-name normalisation these cover live here,
// in config: internal/hooks owns the Hook type and its execution, but the
// config file's shape — which events exist, and what makes a hook entry
// valid — is this package's. They moved with the type (see hooks.Hook).

func TestValidateHooksInvalidRegex(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Hooks: map[string][]HookConfig{
			hooks.EventPreToolUse: {
				{Command: "true", Matcher: "[invalid"},
			},
		},
	}
	err := cfg.ValidateHooks()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid matcher regex")
}

func TestValidateHooksEmptyCommand(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Hooks: map[string][]HookConfig{
			hooks.EventPreToolUse: {
				{Command: ""},
			},
		},
	}
	err := cfg.ValidateHooks()
	require.Error(t, err)
	require.Contains(t, err.Error(), "command is required")
}

func TestValidateHooksNormalizesEventNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"canonical", "PreToolUse"},
		{"lowercase", "pretooluse"},
		{"snake_case", "pre_tool_use"},
		{"upper_snake", "PRE_TOOL_USE"},
		{"mixed_case", "preToolUse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{
				Hooks: map[string][]HookConfig{
					tt.input: {
						{Command: "true"},
					},
				},
			}
			require.NoError(t, cfg.ValidateHooks())
			require.Len(t, cfg.Hooks[hooks.EventPreToolUse], 1)
		})
	}
}

// TestValidateHooksMergesCollidingEventNamesInDeterministicOrder guards
// against a bug where a config file carrying more than one spelling of the
// same hook event (e.g. both "pre_tool_use" and "PRE_TOOL_USE", neither of
// which is hooks.EventPreToolUse's own canonical spelling) had its hooks
// merged in map iteration order — which Go randomizes per run — so the
// merged list, and therefore hook execution order, varied from run to
// run for the exact same config file.
func TestValidateHooksMergesCollidingEventNamesInDeterministicOrder(t *testing.T) {
	t.Parallel()

	for range 20 {
		cfg := &Config{
			Hooks: map[string][]HookConfig{
				"pre_tool_use":   {{Command: "first"}},
				"PRE_TOOL_USE":   {{Command: "second"}},
				"pretooluse":     {{Command: "third"}},
				"Pre_Tool_Use_x": {{Command: "unrelated"}}, // does not normalize; left alone
			},
		}
		require.NoError(t, cfg.ValidateHooks())

		merged := cfg.Hooks[hooks.EventPreToolUse]
		require.Len(t, merged, 3)
		commands := make([]string, len(merged))
		for i, h := range merged {
			commands[i] = h.Command
		}
		require.Equal(t, []string{"second", "first", "third"}, commands,
			"merge order must be a fixed function of the input, not map iteration order")
	}
}
