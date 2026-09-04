package thread_test

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// TestManager_ThreadQuestionRequestReachesTheParentStream is the
// regression test for finding 1: a thread runs in an isolated App with
// its own question service, and question.Service.Ask blocks its caller
// with no timeout, exactly like permission.Service.Request. The relay
// that rescues the permissions case (forwardPermissions) had no sibling
// for questions at all, so a thread calling the question tool hung
// forever with nothing on screen to explain why.
func TestManager_ThreadQuestionRequestReachesTheParentStream(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parent := newTestManagerWithParentApp(t, repo)
	events := parent.Events(t.Context())

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "asks-a-question",
		Goal:        "implement the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	handle := spawner.handleFor(st.WorktreePath)
	require.NotNil(t, handle)

	go func() {
		_, _ = handle.App().Questions.Ask(t.Context(), question.Request{
			SessionID: st.SessionID,
			Questions: []question.Question{{
				Type:        question.TypeYesNo,
				Text:        "Proceed?",
				Description: "Confirm before continuing.",
			}},
		})
	}()

	req := awaitQuestionRequest(t, events)
	require.Len(t, req.Questions, 1)
	require.Equal(t, "Proceed?", req.Questions[0].Text)
}

// TestManager_QuestionServicesRoutesToTheThreadThatIsWaiting covers the
// return path: the parent displays the question but does not hold it, so
// answering has to reach the service actually blocked in Ask.
func TestManager_QuestionServicesRoutesToTheThreadThatIsWaiting(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parent := newTestManagerWithParentApp(t, repo)
	events := parent.Events(t.Context())

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "waiting-thread",
		Goal:        "implement the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	handle := spawner.handleFor(st.WorktreePath)
	require.NotNil(t, handle)

	answered := make(chan []question.Answer, 1)
	go func() {
		answers, _ := handle.App().Questions.Ask(t.Context(), question.Request{
			SessionID: st.SessionID,
			Questions: []question.Question{{
				Type:        question.TypeYesNo,
				Text:        "Proceed?",
				Description: "Confirm before continuing.",
			}},
		})
		answered <- answers
	}()

	req := awaitQuestionRequest(t, events)

	var routed question.Service
	for _, svc := range mgr.QuestionServices() {
		if svc == handle.App().Questions {
			routed = svc
		}
	}
	require.NotNil(t, routed, "a live thread's question service must be discoverable for routing")
	require.NotSame(t, parent.Questions, routed, "and it must not be the parent's own")

	yes := true
	require.True(t, routed.Answer(req.ID, []question.Answer{{QuestionID: req.Questions[0].ID, Yes: &yes}}))

	select {
	case answers := <-answered:
		require.Len(t, answers, 1)
		require.NotNil(t, answers[0].Yes)
		require.True(t, *answers[0].Yes)
	case <-time.After(5 * time.Second):
		t.Fatal("the thread stayed blocked after its question was answered")
	}
}

// awaitQuestionRequest pulls the next question request off a parent
// workspace's event stream, skipping unrelated events the same way
// awaitPermissionRequest does.
func awaitQuestionRequest(t *testing.T, events <-chan pubsub.Event[any]) question.Request {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			if inner, ok := ev.Payload.(pubsub.Event[question.Request]); ok {
				return inner.Payload
			}
		case <-deadline:
			t.Fatal("no question request reached the parent workspace's event stream")
		}
	}
}
