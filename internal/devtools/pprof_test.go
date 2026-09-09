package devtools

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStartPprof_ServesWhenAddressGiven is the whole contract: with the
// variable set, the goroutine dump that the TUI cannot otherwise produce is
// one HTTP GET away.
func TestStartPprof_ServesWhenAddressGiven(t *testing.T) {
	t.Setenv(PprofEnvVar, "localhost:0")

	// Port 0 asks the kernel for a free one, so the test never collides
	// with whatever else is listening on the machine.
	addr, stop := StartPprof()
	defer stop()
	require.NotEmpty(t, addr, "the server reports the address it bound")

	var body []byte
	// The listener is up before StartPprof returns, but Serve runs on its
	// own goroutine; retry briefly rather than race it.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/debug/pprof/goroutine?debug=1") //nolint:noctx // test client, no cancellation to thread through
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)

	require.Contains(t, string(body), "goroutine profile",
		"the endpoint answers with a real profile, not an error page")
}

// TestStartPprof_SilentWhenUnset guards the default: a build that nobody
// asked to profile must not open a port.
func TestStartPprof_SilentWhenUnset(t *testing.T) {
	t.Setenv(PprofEnvVar, "")

	addr, stop := StartPprof()
	defer stop()

	require.Empty(t, addr, "nothing was bound")
}
