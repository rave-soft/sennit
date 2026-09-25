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
	require.True(t, s.Answer("first", []Answer{{QuestionID: "first-q1", FillInText: "because"}}))
	require.NoError(t, <-first)
}

// TestCancelTwiceIsANoOpNotAPanic covers two clients dismissing the same
// form, or a cancel racing a session teardown: closing an already-closed
// channel is fatal, so the second call has to find nothing to close.
func TestAnswerRejectsStaleBatch(t *testing.T) {
	t.Parallel()

	s := NewService()
	done := make(chan error, 1)
	go func() {
		_, err := s.Ask(context.Background(), testRequest("current"))
		done <- err
	}()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pendingID == "current"
	}, 2*time.Second, 5*time.Millisecond)

	require.False(t, s.Answer("stale", []Answer{{QuestionID: "current-q1"}}))
	require.True(t, s.Answer("current", []Answer{{QuestionID: "current-q1", FillInText: "current answer"}}))
	require.NoError(t, <-done)
}

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

	require.True(t, s.Cancel("cancel-me"))
	require.False(t, s.Cancel("cancel-me"), "the second cancel has nothing left to cancel")
	require.False(t, s.Answer("cancel-me", []Answer{{QuestionID: "cancel-me-q1"}}))
	require.ErrorIs(t, <-done, ErrCancelled)
}

// TestCancelChecksTheBatchID is the regression test for finding 1: Cancel
// used to take down whatever question happened to be pending, regardless
// of which batch ID the caller meant to cancel. With two batches never
// simultaneously pending on one service (only one can be, by design), the
// check that matters is that a stale or unrelated ID is refused and the
// real pending question is left blocked, exactly like Answer already does.
func TestCancelChecksTheBatchID(t *testing.T) {
	t.Parallel()

	s := NewService()

	done := make(chan error, 1)
	go func() {
		_, err := s.Ask(context.Background(), testRequest("a"))
		done <- err
	}()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending != nil
	}, 2*time.Second, 5*time.Millisecond)

	require.False(t, s.Cancel("b"), "a non-matching batch ID must not cancel the pending question")
	select {
	case err := <-done:
		t.Fatalf("Ask for %q resolved after an unrelated Cancel(%q): %v", "a", "b", err)
	case <-time.After(50 * time.Millisecond):
		// Still blocked, as expected.
	}

	require.True(t, s.Cancel("a"), "the matching batch ID must cancel it")
	require.ErrorIs(t, <-done, ErrCancelled)
}

// TestRequestValidateAcceptsTheTestShape guards the fixture above: a
// validation failure would make Ask return before installing anything,
// and the pending-state tests would then fail for the wrong reason.
func TestRequestValidateAcceptsTheTestShape(t *testing.T) {
	t.Parallel()
	require.NoError(t, testRequest("x").Validate())
}

// TestActiveRequest_ReportsTheQuestionStillWaiting pins what a relay
// installed after Ask has already published needs. A request is announced
// exactly once, and Ask blocks with no timeout, so a listener that starts
// later has no other way to learn a question is outstanding — and the
// caller behind it waits forever. This is the question service's half of
// the accessor the permission service has carried for the same reason.
func TestActiveRequest_ReportsTheQuestionStillWaiting(t *testing.T) {
	t.Parallel()
	svc := NewService()

	_, ok := svc.ActiveRequest()
	require.False(t, ok, "nothing is pending before the first Ask")

	asked := make(chan struct{})
	go func() {
		close(asked)
		_, _ = svc.Ask(t.Context(), Request{
			Questions: []Question{{Type: TypeYesNo, Text: "proceed?", Description: "confirm the operation"}},
		})
	}()
	<-asked

	var req Request
	require.Eventually(t, func() bool {
		var found bool
		req, found = svc.ActiveRequest()
		return found
	}, 2*time.Second, 5*time.Millisecond, "the waiting question must be reported")
	require.Len(t, req.Questions, 1)
	require.Equal(t, "proceed?", req.Questions[0].Text)

	require.True(t, svc.Cancel(req.ID))
	require.Eventually(t, func() bool {
		_, found := svc.ActiveRequest()
		return !found
	}, 2*time.Second, 5*time.Millisecond, "a cancelled question must stop being reported")
}
