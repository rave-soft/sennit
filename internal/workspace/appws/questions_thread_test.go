package appws

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// TestAppWorkspace_QuestionAnswerRoutesToTheThreadHoldingIt is the
// question-side mirror of TestAppWorkspace_PermissionAnswerRoutesToTheThreadHoldingIt:
// a thread's question tool is raised inside its own isolated workspace and
// only displayed here, so before finding 1 was fixed, answering against
// this workspace's own question service found no such request and quietly
// did nothing, leaving the thread's question tool blocked forever.
func TestAppWorkspace_QuestionAnswerRoutesToTheThreadHoldingIt(t *testing.T) {
	ws, mgr := newTestThreadAppWorkspace(t)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "asks-a-question",
		Goal:        "do the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	services := mgr.QuestionServices()
	require.Len(t, services, 1, "precondition: exactly one live thread")
	threadQuestions := services[0]

	raised := threadQuestions.Subscribe(t.Context())
	answered := make(chan []question.Answer, 1)
	go func() {
		answers, _ := threadQuestions.Ask(t.Context(), question.Request{
			SessionID: st.SessionID,
			Questions: []question.Question{{
				Type:        question.TypeYesNo,
				Text:        "Proceed?",
				Description: "Confirm before continuing.",
			}},
		})
		answered <- answers
	}()

	var req question.Request
	select {
	case ev := <-raised:
		req = ev.Payload
	case <-time.After(5 * time.Second):
		t.Fatal("the thread never raised its question request")
	}

	yes := true
	require.True(t, ws.QuestionAnswer(req.ID, []question.Answer{{QuestionID: req.Questions[0].ID, Yes: &yes}}),
		"answering through the parent workspace must reach the thread's own service")

	select {
	case answers := <-answered:
		require.Len(t, answers, 1)
		require.True(t, *answers[0].Yes)
	case <-time.After(5 * time.Second):
		t.Fatal("the thread stayed blocked after the parent answered its question")
	}
}

// TestAttachedThread_QuestionAnswerReachesTheParentThatRaisedIt is the
// question-side mirror of TestAttachedThread_PermissionAnswerReachesTheParentThatRaisedIt:
// while the user is drilled into a thread, a question the parent
// workspace raised behind it is relayed onto that screen too, and
// answering it there must still reach the parent's own service.
func TestAttachedThread_QuestionAnswerReachesTheParentThatRaisedIt(t *testing.T) {
	ws, mgr := newTestThreadAppWorkspace(t)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "attached",
		Goal:        "do the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	attached, detach, err := ws.AttachThread(t.Context(), st.ID)
	require.NoError(t, err)
	t.Cleanup(detach)

	parentQuestions := ws.app.Questions
	raised := parentQuestions.Subscribe(t.Context())
	answered := make(chan []question.Answer, 1)
	go func() {
		answers, _ := parentQuestions.Ask(t.Context(), question.Request{
			SessionID: "parent-session",
			Questions: []question.Question{{
				Type:        question.TypeYesNo,
				Text:        "Proceed?",
				Description: "Confirm before continuing.",
			}},
		})
		answered <- answers
	}()

	var req question.Request
	select {
	case ev := <-raised:
		req = ev.Payload
	case <-time.After(5 * time.Second):
		t.Fatal("the parent never raised its question request")
	}

	yes := true
	require.True(t, attached.QuestionAnswer(req.ID, []question.Answer{{QuestionID: req.Questions[0].ID, Yes: &yes}}),
		"answering on the thread's screen must still reach the service that raised the question")

	select {
	case answers := <-answered:
		require.Len(t, answers, 1)
		require.True(t, *answers[0].Yes)
	case <-time.After(5 * time.Second):
		t.Fatal("the parent stayed blocked after its question was answered from the thread's screen")
	}
}
