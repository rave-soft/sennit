package workspace

import (
	"testing"

	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestMaxBackgroundJobsMatchesShell pins the contract's own copy of
// MaxBackgroundJobs to internal/shell's, since the constant is duplicated
// rather than aliased to keep internal/shell (and the bash interpreter it
// links) out of the UI's transitive dependency set. This test, not the
// import it replaces, is what catches the two values drifting apart.
func TestMaxBackgroundJobsMatchesShell(t *testing.T) {
	t.Parallel()
	require.Equal(t, shell.MaxBackgroundJobs, MaxBackgroundJobs)
}
