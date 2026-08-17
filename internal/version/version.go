package version

import (
	"os"
	"runtime/debug"
	"strconv"
)

// Build-time parameters set via -ldflags.

// defaultVersion is what Version reads as when the linker set nothing: an
// unstamped build. It is the one value the build-info fallback may replace.
const defaultVersion = "devel"

var (
	Version = defaultVersion
	Commit  = "unknown"
	// BuildID is a unique identifier for this build. For release builds it
	// equals Commit; for development builds (go run / go build without
	// ldflags) it is derived from the executable's modification time, which
	// changes on every recompilation.
	BuildID = ""
)

// A user may install sennit using `go install github.com/rave-soft/sennit@latest`
// without -ldflags, in which case the version above is left at its default. The
// embedded build version covers that case.
//
// It must not override a version the linker did set. Since Go 1.24 the
// toolchain stamps Main.Version from VCS state for a plain `go build` too — a
// pseudo-version like v0.0.0-20260817014303-d1d059c0ea94 — so adopting it
// unconditionally made a release binary report that instead of its tag.
func init() {
	if Version == defaultVersion {
		if info, ok := debug.ReadBuildInfo(); ok {
			mainVersion := info.Main.Version
			if mainVersion != "" && mainVersion != "(devel)" {
				Version = mainVersion
			}
		}
	}

	// Derive BuildID when not set via ldflags.
	if BuildID == "" {
		BuildID = deriveBuildID()
	}
}

// deriveBuildID uses the running executable's modification time as a unique
// build fingerprint. This changes on every recompilation (including `go run`),
// making it reliable for detecting stale servers during development.
func deriveBuildID() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "unknown"
	}
	return strconv.FormatInt(fi.ModTime().UnixNano(), 36)
}
