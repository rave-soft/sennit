package toolmeta_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/toolmeta"
	"github.com/stretchr/testify/require"
)

// readOnlyBlock pulls the permissions command out of the "read-only set"
// section of the tool reference. The section names itself after
// TaskReadOnlyNames, so it is a claim about this package that nothing
// checked: an audit found the documentation carrying three different
// versions of this list, one of which promised a tool that writes.
var readOnlyBlock = regexp.MustCompile("(?s)## The read-only set.*?```bash\n(.*?)```")

// TestDocsReadOnlySetMatchesRegistry keeps the documented list and the
// registry from drifting apart again. A name that appears here and not in
// the registry tells someone to allow a tool the delegate will not get; a
// name in the registry and not here quietly leaves a safe tool prompting
// forever. Neither is visible without reading both files side by side,
// which is exactly why this is a test.
func TestDocsReadOnlySetMatchesRegistry(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "docs", "reference", "tools.md")
	contents, err := os.ReadFile(path)
	require.NoError(t, err)

	match := readOnlyBlock.FindSubmatch(contents)
	require.NotNil(t, match, "the read-only set section of %s no longer has a bash block; update this test with it", path)

	documented := parsePermissionsAllow(t, string(match[1]))
	registry := toolmeta.TaskReadOnlyNames()

	slices.Sort(documented)
	slices.Sort(registry)
	require.Equal(t, registry, documented,
		"docs/reference/tools.md and toolmeta.TaskReadOnlyNames disagree about the read-only set")
}

// parsePermissionsAllow reads the tool names out of a "permissions allow"
// command, following the line continuations the block is wrapped with.
func parsePermissionsAllow(t *testing.T, block string) []string {
	t.Helper()

	fields := strings.Fields(strings.ReplaceAll(block, "\\\n", " "))
	require.GreaterOrEqual(t, len(fields), 2)
	require.Equal(t, "permissions", fields[0])
	require.Equal(t, "allow", fields[1])
	return fields[2:]
}
