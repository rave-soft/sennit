package doctor

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentProblems_FlagsMissingClipboardHelper(t *testing.T) {
	t.Parallel()

	problems := environmentProblems([]string{"xclip"})

	require.Len(t, problems, 1)
	require.Equal(t, config.AreaEnvironment, problems[0].Area)
	require.Equal(t, config.SeverityWarn, problems[0].Severity)
	require.Equal(t, "clipboard", problems[0].Subject)
	require.NotEmpty(t, problems[0].Hint)
}

func TestEnvironmentProblems_SaysNothingWhenAHelperIsInstalled(t *testing.T) {
	t.Parallel()

	require.Empty(t, environmentProblems(nil))
}

// TestSkillProblems_ReportsOnlyFailedSkills covers what had no test while
// this lived in internal/config: a skill that loaded is not a problem, and
// a skill that did not is reported once, with the parse error attached —
// that error text is the whole point, since the usual cause is an unquoted
// colon in the frontmatter description and nothing else says so.
func TestSkillProblems_ReportsOnlyFailedSkills(t *testing.T) {
	t.Parallel()

	problems := SkillProblems([]*skills.SkillState{
		nil,
		{Name: "fine", State: skills.StateNormal},
		{Name: "broken", State: skills.StateError, Path: "/w/.sennit/skills/broken/SKILL.md", Err: errors.New("yaml: mapping values are not allowed")},
	})

	require.Len(t, problems, 1)
	require.Equal(t, config.AreaSkill, problems[0].Area)
	require.Equal(t, config.SeverityError, problems[0].Severity)
	require.Equal(t, "broken", problems[0].Subject)
	require.Contains(t, problems[0].Message, "yaml: mapping values are not allowed")
	require.Contains(t, problems[0].Hint, "/w/.sennit/skills/broken/SKILL.md")
}

// TestSkillProblems_NamesTheDirectoryWhenTheSkillHasNoName is the case the
// fallback exists for: a skill whose frontmatter failed to parse has no
// name to report, so the person is told the directory they can actually
// find on disk.
func TestSkillProblems_NamesTheDirectoryWhenTheSkillHasNoName(t *testing.T) {
	t.Parallel()

	path := filepath.Join("/w", ".sennit", "skills", "unnamed", "SKILL.md")
	problems := SkillProblems([]*skills.SkillState{
		{State: skills.StateError, Path: path, Err: errors.New("boom")},
	})

	require.Len(t, problems, 1)
	require.Equal(t, "unnamed", problems[0].Subject)
}

func TestSkillProblems_EmptySnapshot(t *testing.T) {
	t.Parallel()

	require.Empty(t, SkillProblems(nil))
}
