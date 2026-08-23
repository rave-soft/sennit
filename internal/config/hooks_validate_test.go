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
