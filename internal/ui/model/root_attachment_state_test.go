package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestThreadAttachmentStateReleaseClearsBeforeTeardown(t *testing.T) {
	t.Parallel()

	var stopped, detached int
	state := threadAttachmentState{
		thread: &threadAttachment{
			stop:   func() { stopped++ },
			detach: func() { detached++ },
		},
	}

	cmd := state.release()
	require.Nil(t, state.thread, "late thread events must be rejected before teardown can block")
	require.Zero(t, stopped, "teardown must not run on the Update goroutine")
	require.Zero(t, detached, "teardown must not run on the Update goroutine")
	require.NotNil(t, cmd)

	msg := cmd()
	require.Nil(t, msg)
	require.Equal(t, 1, stopped)
	require.Equal(t, 1, detached)
	require.Nil(t, state.release(), "a released attachment cannot be torn down twice")
}

func TestThreadAttachmentStateReleaseWithoutAttachment(t *testing.T) {
	t.Parallel()

	var state threadAttachmentState
	require.Nil(t, state.release())
}

func TestThreadAttachmentStateCleanupReleasesAttachment(t *testing.T) {
	t.Parallel()

	var stopped, detached int
	state := threadAttachmentState{
		thread: &threadAttachment{
			stop:   func() { stopped++ },
			detach: func() { detached++ },
		},
	}

	state.cleanup()
	require.Nil(t, state.thread)
	require.Equal(t, 1, stopped)
	require.Equal(t, 1, detached)
}
