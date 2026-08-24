package agent

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// TestDispatchDecision_QueuedAgentPromptDoesNotSignalUserInput is the
// regression test for the busy branch of dispatchDecision: a prompt an
// agent queued on the person's behalf (PromptOrigin == message.OriginAgent
// - a delegation follow-up, say) must still be queued behind the active
// turn, but it must not release a tool that turn has parked on
// tools.WaitForUserInput. Only the person's own words - PromptOrigin left
// at its default, message.OriginPerson - may cut that wait short. See the
// doc comments on dispatcher.userInput, signalUserInput, and
// tools.WaitForUserInput for the doctrine this enforces.
func TestDispatchDecision_QueuedAgentPromptDoesNotSignalUserInput(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &concurrencyProbeModel{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// Arm the wait signal before the active run starts, exactly as
	// runTurn does when it installs tools.WithUserInput on the run
	// context.
	waitSignal := sa.userInputChan(sess.ID)

	done := make(chan struct{})
	go func() {
		_, _ = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "first"})
		close(done)
	}()

	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		close(model.release)
		<-done
		t.Fatal("no run became active")
	}
	defer func() {
		close(model.release)
		<-done
	}()

	// An agent-originated prompt queues behind the active turn, but must
	// not signal the wait.
	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID:    sess.ID,
		Prompt:       "delegation follow-up",
		PromptOrigin: message.OriginAgent,
	})
	require.NoError(t, err)

	select {
	case <-waitSignal:
		t.Fatal("an agent-queued prompt must not release a tool waiting on the person")
	case <-time.After(50 * time.Millisecond):
	}

	// A prompt with the default origin (the person) still does.
	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "person follow-up",
	})
	require.NoError(t, err)

	select {
	case <-waitSignal:
	case <-time.After(5 * time.Second):
		t.Fatal("a person-queued prompt must release a tool waiting on the person")
	}
}
