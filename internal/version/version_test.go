package version

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLdflagsVersionSurvivesBuildInfo builds the real binary with a stamped
// version and asks it what it is.
//
// The trap this guards is silent: since Go 1.24 a plain `go build` stamps
// Main.Version with a VCS pseudo-version, so an init that adopted build info
// unconditionally replaced the tag the release pipeline had just linked in.
// Nothing fails — the binary simply reports the wrong version everywhere,
// including in the update banner and in bug reports.
func TestLdflagsVersionSurvivesBuildInfo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}

	binary := filepath.Join(t.TempDir(), "sennit")
	build := exec.Command("go", "build",
		"-ldflags", "-X github.com/rave-soft/sennit/internal/version.Version=v9.9.9",
		"-o", binary, "github.com/rave-soft/sennit")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	reported, err := exec.Command(binary, "--version").Output()
	require.NoError(t, err)
	require.Contains(t, string(reported), "v9.9.9")
	require.NotContains(t, string(reported), "v0.0.0-",
		"a VCS pseudo-version means build info overrode the linked-in version")
}

// TestUnstampedBuildFallsBackToBuildInfo: an unstamped build is the
// `go install ...@latest` case, where the module version is all there is.
func TestUnstampedBuildFallsBackToBuildInfo(t *testing.T) {
	require.NotEmpty(t, Version)
	require.False(t, strings.HasPrefix(Version, "-"))
}
