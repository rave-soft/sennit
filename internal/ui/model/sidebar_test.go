package model

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestBackgroundJobsInfo(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	const width = 24
	info := backgroundJobsInfo(&sty, shell.BackgroundJobCounts{Active: 3, Completed: 7}, width)
	plain := ansi.Strip(info)

	require.Contains(t, plain, "Background Jobs")
	require.Contains(t, plain, "Active 3/50")
	require.NotContains(t, plain, "Completed")
	for line := range strings.Lines(info) {
		require.LessOrEqual(t, lipgloss.Width(strings.TrimSuffix(line, "\n")), width)
	}
}
