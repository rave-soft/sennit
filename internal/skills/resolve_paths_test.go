package skills

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ResolvePaths used to gate resolution on strings.HasPrefix(expanded,
// "$"), which only ever fired for a reference at the very start of the
// path. A reference in the middle — the shape an admin actually writes,
// "/data/$USER/skills" — was handed to the walker verbatim.
func TestResolvePaths_ResolvesAReferenceAnywhereInThePath(t *testing.T) {
	t.Parallel()

	cfg := DiscoveryConfig{
		SkillsPaths: []string{"/data/$USER/skills", "$SKILLS_DIR/extra"},
		Resolver: func(v string) (string, error) {
			v = strings.ReplaceAll(v, "$USER", "alice")
			return strings.ReplaceAll(v, "$SKILLS_DIR", "/opt/skills"), nil
		},
	}

	require.Equal(t,
		[]string{"/data/alice/skills", "/opt/skills/extra"},
		cfg.ResolvePaths())
}

// A reference that cannot be resolved is dropped, not passed through. The
// literal "$SKILLS_DIR" is not a directory name: keeping it meant walking
// a relative path off the process's cwd, picking up whatever sat there
// with no sign the reference had failed.
func TestResolvePaths_DropsUnresolvableEntries(t *testing.T) {
	t.Parallel()

	cfg := DiscoveryConfig{
		SkillsPaths: []string{"/plain/skills", "$SKILLS_DIR/extra"},
		Resolver: func(v string) (string, error) {
			return "", fmt.Errorf("no such variable in %s", v)
		},
	}

	got := cfg.ResolvePaths()
	require.Equal(t, []string{"/plain/skills"}, got)
	for _, p := range got {
		require.NotContains(t, p, "$", "an unresolved reference must never reach the walker")
	}
}

// A path with no reference at all never touches the resolver, so a
// workspace without one keeps working.
func TestResolvePaths_LeavesPlainPathsAlone(t *testing.T) {
	t.Parallel()

	cfg := DiscoveryConfig{SkillsPaths: []string{"/plain/skills"}}
	require.Equal(t, []string{"/plain/skills"}, cfg.ResolvePaths())
}
