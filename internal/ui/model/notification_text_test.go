package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestNotificationTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		workingDir string
		want       string
	}{
		{name: "empty", workingDir: "", want: "Braid"},
		{name: "root", workingDir: "/", want: "Braid"},
		{name: "project dir", workingDir: "/home/user/my-project", want: "Braid — my-project"},
		{name: "trailing slash", workingDir: "/home/user/my-project/", want: "Braid — my-project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, notificationTitle(tt.workingDir))
		})
	}
}

func TestNotificationBodyTaskFinished(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Task finished", notificationBodyTaskFinished(""))
	require.Equal(t, "Task finished: Fix the login bug", notificationBodyTaskFinished("Fix the login bug"))

	long := strings.Repeat("a", maxNotificationBodyLen+50)
	got := notificationBodyTaskFinished(long)
	require.True(t, strings.HasPrefix(got, "Task finished: "))
	require.True(t, strings.HasSuffix(got, "…"))
	require.LessOrEqual(t, ansi.StringWidth(strings.TrimPrefix(got, "Task finished: ")), maxNotificationBodyLen)
}

func TestNotificationBodyTaskFailed(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Task failed", notificationBodyTaskFailed(""))
	require.Equal(t, "Task failed", notificationBodyTaskFailed("   "))
	require.Equal(t, "Task failed: connection refused", notificationBodyTaskFailed("connection refused"))
}

func TestNotificationBodyPermission(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Permission needed: bash", notificationBodyPermission("bash"))
}

func TestNotificationBodyQuestions(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Input needed: 1 question", notificationBodyQuestions(1))
	require.Equal(t, "Input needed: 3 questions", notificationBodyQuestions(3))
}
