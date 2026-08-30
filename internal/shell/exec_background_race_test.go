package shell

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Shell.exec used to collect stdout/stderr into plain bytes.Buffers.
// mvdan.cc/sh does not wait for a backgrounded `&` job before Run
// returns, so that job can still be writing while exec reads the buffers
// out — the same race Run and the hook runner already use SyncBuffer to
// avoid. Run under -race to catch a regression to bytes.Buffer.
func TestShellExec_BackgroundedJobDoesNotRaceOnOutputBuffers(t *testing.T) {
	t.Parallel()

	sh := NewShell(&Options{WorkingDir: t.TempDir()})

	stdout, _, err := sh.Exec(t.Context(), `(sleep 0.1; echo late) & echo early`)
	require.NoError(t, err)
	require.Contains(t, stdout, "early")

	// Exec has already read the buffers above; the backgrounded job
	// writes into them after that. Stay alive long enough for that write
	// to actually land, or there is no second access for the detector to
	// compare the read against.
	time.Sleep(400 * time.Millisecond)
}
