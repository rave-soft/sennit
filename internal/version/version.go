package version

import (
	"runtime/debug"
)

// Build-time parameters set via -ldflags.

// defaultVersion is what Version reads as when the linker set nothing: an
// unstamped build. It is the one value the build-info fallback may replace.
const defaultVersion = "devel"

var (
	Version = defaultVersion
	// Commit is stamped by the release build (see .goreleaser.yml). Nothing
	// reads it today; it is kept because a released binary that cannot say
	// which commit it is built from is worth less than the line it costs.
	Commit = "unknown"
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
}
