package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSubstituteArgsPrefersTheLongerName pins the ordering. Map iteration
// is random, so with both $FILE and $FILE_PATH defined, whichever came out
// first won: substituting $FILE into "$FILE_PATH" leaves "<value>_PATH",
// and the same command produced different text on different runs.
func TestSubstituteArgsPrefersTheLongerName(t *testing.T) {
	t.Parallel()

	args := map[string]string{"FILE": "a.go", "FILE_PATH": "/tmp/a.go"}
	for range 20 {
		require.Equal(t, "/tmp/a.go and a.go",
			substituteArgs("$FILE_PATH and $FILE", args))
	}
}
