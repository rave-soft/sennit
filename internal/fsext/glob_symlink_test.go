package fsext

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestVisitGlobGitignoreAware_DoesNotFollowSymlinkEscape pins that a glob
// stays inside its search root even when a symlink inside that root points
// out of it.
//
// The property rests on a single word — fastwalk.Config{Follow: false} in
// VisitGlobGitignoreAware — which nothing else would notice being flipped.
// It used to be covered against globFiles in internal/agent/tools, an
// older implementation that has since been superseded and deleted; the
// guarantee moved to this function, so the test does too.
func TestVisitGlobGitignoreAware_DoesNotFollowSymlinkEscape(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.go"), []byte("x"), 0o644))

	project := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(project, "in.go"), []byte("x"), 0o644))
	if err := os.Symlink(outside, filepath.Join(project, "escape")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	// fastwalk visits concurrently, so the append needs a lock even here,
	// where the tree is small enough that the race rarely fires.
	var (
		mu  sync.Mutex
		got []string
	)
	require.NoError(t, VisitGlobGitignoreAware(context.Background(), "**/*.go", project,
		func(path string, _ time.Time) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, path)
		}))

	require.NotEmpty(t, got, "the file inside the root must still be found")
	for _, p := range got {
		require.NotContains(t, p, "secret.go", "glob followed a symlink out of the search root")
	}
}
