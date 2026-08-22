package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// jsonPath renders p as a JSON string literal, quotes included, for
// splicing into a hand-written JSON fixture.
//
// Never concatenate a filesystem path into JSON directly: on Windows a
// path like C:\Users\RUNNER~1\... puts "\U" into the string, which is an
// illegal JSON escape, and the fixture fails to parse before the test
// reaches whatever it meant to check. That is not hypothetical -- it took
// out four tests in this package on the Windows CI leg, and the same
// class had already broken the workspace-confinement tests in
// internal/agent/tools.
func jsonPath(t *testing.T, p string) string {
	t.Helper()
	encoded, err := json.Marshal(p)
	require.NoError(t, err)
	return string(encoded)
}
