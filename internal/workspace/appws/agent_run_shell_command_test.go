package appws

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestAppWorkspace_AgentRunShellCommand_StreamingErrorSurfaces is the
// regression test for the `err != nil && onProgress == nil` asymmetry in
// AgentRunShellCommand: an error from the streaming path used to be
// swallowed, so a command that failed to even start was reported back to
// the caller as a (zero-value) success. runAndCaptureStream is stubbed
// out here because the real shell.RunAndCaptureStream folds every actual
// failure into CaptureResult.ExitCode and never itself returns a non-nil
// error — the only way to exercise this branch is to simulate one.
//
// It also asserts the streamed output is not persisted when the run
// fails, matching RunAndPersist's own convention of skipping persist on
// error.
func TestAppWorkspace_AgentRunShellCommand_StreamingErrorSurfaces(t *testing.T) {
	orig := runAndCaptureStream
	t.Cleanup(func() { runAndCaptureStream = orig })

	wantErr := errors.New("boom: command failed to start")
	runAndCaptureStream = func(context.Context, shell.RunOptions, func(string)) (shell.CaptureResult, error) {
		return shell.CaptureResult{}, wantErr
	}

	sessions, messages := newRealSessionAgentEnv(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.SetSessionsForTest(sessions)
	a.SetMessagesForTest(messages)
	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	sess, err := aw.CreateSession(t.Context(), "shell session")
	require.NoError(t, err)

	resp, err := aw.AgentRunShellCommand(t.Context(), sess.ID, "some-command", 80, func(string) {}, false)
	require.ErrorIs(t, err, wantErr)
	require.Zero(t, resp)

	msgs, err := aw.ListMessages(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, msgs, "a failed streamed command must not be persisted")
}
