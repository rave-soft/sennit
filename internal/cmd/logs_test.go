package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/nxadm/tail"
	"github.com/stretchr/testify/require"
)

// TestRunFollowLoop_ClosedLinesChannelReturnsErrInsteadOfPanicking guards
// against a nil deref: nxadm/tail closes t.Lines when its internal
// goroutine dies (e.g. the watched file was removed), rather than sending
// a final *Line on it. A bare `line := <-t.Lines` receive after that
// yields a nil *Line, and dereferencing line.Err panicked the whole
// process instead of ending the command with an error.
func TestRunFollowLoop_ClosedLinesChannelReturnsErrInsteadOfPanicking(t *testing.T) {
	t.Parallel()

	tt := &tail.Tail{Lines: make(chan *tail.Line)}
	killErr := errors.New("simulated tail failure")
	tt.Kill(killErr)
	close(tt.Lines)

	require.NotPanics(t, func() {
		err := runFollowLoop(context.Background(), tt)
		require.ErrorIs(t, err, killErr)
	})
}

// TestRunFollowLoop_ContextCancelReturnsNil covers the ordinary shutdown
// path: a cancelled context ends the loop cleanly, with no error, even
// though t.Lines is never closed.
func TestRunFollowLoop_ContextCancelReturnsNil(t *testing.T) {
	t.Parallel()

	tt := &tail.Tail{Lines: make(chan *tail.Line)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, runFollowLoop(ctx, tt))
}
