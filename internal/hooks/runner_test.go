package hooks

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestRunner_DedupKeyIncludesFullConfig verifies that Run only collapses
// hooks whose entire config is identical, not merely hooks that happen to
// share a Command string. Deduping on Command alone would drop one of two
// same-command hooks that differ in name/timeout/matcher, silently
// applying the survivor's settings to both.
func TestRunner_DedupKeyIncludesFullConfig(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	r := &Runner{
		hooks: []compiledHook{
			{cfg: Hook{Name: "first", Command: "true", Timeout: 5}},
			{cfg: Hook{Name: "second", Command: "true", Timeout: 10}},
			// Exact duplicate of "first": same Name, Command, Timeout,
			// Matcher. This one really should collapse.
			{cfg: Hook{Name: "first", Command: "true", Timeout: 5}},
		},
		runShell: func(context.Context, shell.RunOptions) error {
			calls.Add(1)
			return nil
		},
		abandonFor: abandonGrace,
	}

	agg, err := r.Run(t.Context(), "PreToolUse", "session-1", "bash", "{}")
	require.NoError(t, err)

	// Two distinct hooks (first, second) should each run once; the exact
	// duplicate of "first" should not trigger a third execution.
	require.Equal(t, int32(2), calls.Load())
	require.Len(t, agg.Hooks, 2)

	names := []string{agg.Hooks[0].Name, agg.Hooks[1].Name}
	require.ElementsMatch(t, []string{"first", "second"}, names)
}
