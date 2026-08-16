package testenv

import (
	"os"
	"path/filepath"

	"github.com/rave-soft/sennit/internal/brand"
)

// IsolateGlobalProfile points the global config and data profiles at a
// throwaway directory for the rest of the process, and returns a function
// that removes it.
//
// Call it from a package's TestMain. Tests that reach the global profile
// through config.GlobalConfig, config.GlobalConfigData or config.GlobalDBDir
// otherwise land in the developer's real profile — a package whose tests
// forget to isolate will silently write sessions and model caches into
// ~/.config/sennit. Per-test t.Setenv still overrides what this sets, so an
// individual test can keep using its own directory.
//
// This exists because both failure modes have already happened: some tests
// never isolated at all, and others isolated through the pre-rename
// BRAID_GLOBAL_* variables, which the product stopped reading — leaving the
// isolation in place but inert. A package-wide default fails safe instead.
func IsolateGlobalProfile() func() {
	dir, err := os.MkdirTemp("", brand.Slug+"-test-profile-*")
	if err != nil {
		panic("testenv: create temp profile: " + err.Error())
	}

	set := func(name, value string) {
		if err := os.Setenv(name, value); err != nil {
			// Panicking is the point: a silent failure here would let the
			// package's tests run against the real profile.
			panic("testenv: set " + name + ": " + err.Error())
		}
	}
	set(brand.EnvPrefix+"GLOBAL_CONFIG", filepath.Join(dir, "config"))
	set(brand.EnvPrefix+"GLOBAL_DATA", filepath.Join(dir, "data"))

	return func() { _ = os.RemoveAll(dir) }
}
