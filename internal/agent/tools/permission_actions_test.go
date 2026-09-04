package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

// permissionAction matches the Action field of a permission request built
// anywhere in this package. The word boundary matters: readAction, a
// different field on the file-mutation request that carries prose for a
// staleness message, would otherwise be swept in.
var permissionAction = regexp.MustCompile(`\bAction:\s*"([^"]+)"`)

// TestPermissionActionsAreKnown keeps permission.KnownActions and the
// actions this package actually raises from drifting apart. The list is
// what config.Doctor validates an allowlist entry's second half against,
// so a tool that starts asking with a new verb would otherwise make
// doctor reject the very entry a person needs to write - and a verb
// dropped from the tools would leave doctor accepting an entry that can
// no longer fire.
func TestPermissionActionsAreKnown(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var used []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		contents, err := os.ReadFile(filepath.Clean(name))
		require.NoError(t, err)
		for _, match := range permissionAction.FindAllSubmatch(contents, -1) {
			action := string(match[1])
			if !slices.Contains(used, action) {
				used = append(used, action)
			}
		}
	}
	require.NotEmpty(t, used, "no permission actions found; the pattern this test greps for must have changed")

	for _, action := range used {
		require.True(t, permission.IsKnownAction(action),
			"tools raise permission requests with action %q, which permission.KnownActions omits: doctor would reject a valid allowlist entry naming it", action)
	}
	for _, known := range permission.KnownActions {
		require.Contains(t, used, known,
			"permission.KnownActions lists %q but no tool asks with it: doctor would accept an allowlist entry that can never fire", known)
	}
}
