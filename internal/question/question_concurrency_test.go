package question

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testRequest(id string) Request {
	return Request{
		ID: id,
		Questions: []Question{
			{ID: id + "-q1", Type: TypeFreeText, Text: "why?", Description: "explain the reason"},
		},
	}
}

// TestAskRefusesASecondQuestionWhileOneIsPending pins the state a parallel
// delegation used to destroy. The service holds a single pending channel,
// and a second Ask overwrote it: the first caller was left blocked on a
// channel nobody would send to, and its own deferred cleanup then cleared
// the second one's state on the way out.
func TestAskRefusesASecondQuestionWhileOneIsPending(t *testing.T) {
	t.Parallel()

	s := NewService()

	first := make(chan error, 1)
	go func() {
		_, err := s.Ask(context.Background(), testRequest("first"))
		first <- err
	}()

	// Wait for the first Ask to install itself.
	require.Eventually(t, func() bool {
		return func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.pending != nil
		}()
	}, 2*time.Second, 5*time.Millisecond)

	_, err := s.Ask(context.Background(), testRequest("second"))
	require.ErrorIs(t, err, ErrQuestionPending)

	// The first is still answerable, which is the point: it was not
	// displaced by the refused one.
	require.True(t, s.Answer([]Answer{{QuestionID: "first-q1", FillInText: "because"}}))
	require.NoError(t, <-first)
}

// TestCancelTwiceIsANoOpNotAPanic covers two clients dismissing the same
// form, or a cancel racing a session teardown: closing an already-closed
// channel is fatal, so the second call has to find nothing to close.
func TestCancelTwiceIsANoOpNotAPanic(t *testing.T) {
	t.Parallel()

	s := NewService()

	done := make(chan error, 1)
	go func() {
		_, err := s.Ask(context.Background(), testRequest("cancel-me"))
		done <- err
	}()
	require.Eventually(t, func() bool {
		return func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.pending != nil
		}()
	}, 2*time.Second, 5*time.Millisecond)

	require.True(t, s.Cancel())
	require.False(t, s.Cancel(), "the second cancel has nothing left to cancel")
	require.ErrorIs(t, <-done, ErrCancelled)
}

// TestRequestValidateAcceptsTheTestShape guards the fixture above: a
// validation failure would make Ask return before installing anything,
// and the pending-state tests would then fail for the wrong reason.
func TestRequestValidateAcceptsTheTestShape(t *testing.T) {
	t.Parallel()
	require.NoError(t, testRequest("x").Validate())
}
