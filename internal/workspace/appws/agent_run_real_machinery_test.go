package appws

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

// -- real session/message services, mirroring internal/agent's own
// testEnv helper. --

func newRealSessionAgentEnv(t *testing.T) (sessionstore.Service, messagestore.Service) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)
	return sessionstore.NewService(q, conn, "/test/project"), messagestore.NewService(q)
}

// -- coordinatorOverSessionAgent adapts a real agent.SessionAgent (built
// via the public agent.NewSessionAgent, backed by the actual
// internal/agent accept/dispatch/queue machinery in dispatch.go) to
// satisfy agent.Coordinator.
//
// This is deliberately not the full agent.NewCoordinator: that
// constructor resolves providers/models/tools from config, which would
// require either live network access or a fake registered as a
// catwalk/fantasy provider type — there is no seam for injecting a raw
// fantasy.LanguageModel through it directly. But
// Coordinator.RunAccepted/BeginAccepted/Cancel/CancelAll/
// QueuedPromptsList are thin pass-throughs to the SessionAgent for
// exactly these methods in production (see coordinator.go's run,
// BeginAccepted, Cancel, CancelAll, QueuedPromptsList), so wrapping a
// real SessionAgent here exercises the same accept/queue/cancel code
// AppWorkspace/AgentDispatcher actually drives, without the unrelated
// provider/runtime resolution machinery. --
type coordinatorOverSessionAgent struct {
	sa agent.SessionAgent
}

func (c *coordinatorOverSessionAgent) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.sa.Run(ctx, agent.SessionAgentCall{SessionID: sessionID, Prompt: prompt, Attachments: attachments})
}

func (c *coordinatorOverSessionAgent) RunAccepted(ctx context.Context, accept *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.sa.Run(ctx, agent.SessionAgentCall{SessionID: sessionID, Prompt: prompt, Attachments: attachments, Accepted: accept})
}

func (c *coordinatorOverSessionAgent) Steer(ctx context.Context, call agent.SessionAgentCall) (agent.SteerOutcome, *fantasy.AgentResult, error) {
	return c.sa.Steer(ctx, call)
}

func (c *coordinatorOverSessionAgent) BeginAccepted(sessionID string) *agent.AcceptedRun {
	return c.sa.BeginAccepted(sessionID)
}

func (c *coordinatorOverSessionAgent) Cancel(sessionID string) { c.sa.Cancel(sessionID) }
func (c *coordinatorOverSessionAgent) CancelAll()              { c.sa.CancelAll() }

func (c *coordinatorOverSessionAgent) IsSessionBusy(sessionID string) bool {
	return c.sa.IsSessionBusy(sessionID)
}

func (c *coordinatorOverSessionAgent) IsBusy() bool { return c.sa.IsBusy() }

func (c *coordinatorOverSessionAgent) QueuedPrompts(sessionID string) int {
	return c.sa.QueuedPrompts(sessionID)
}

func (c *coordinatorOverSessionAgent) QueuedPromptsList(sessionID string) []string {
	return c.sa.QueuedPromptsList(sessionID)
}

func (c *coordinatorOverSessionAgent) ClearQueue(sessionID string) { c.sa.ClearQueue(sessionID) }

func (c *coordinatorOverSessionAgent) Summarize(context.Context, string) error { return nil }

func (c *coordinatorOverSessionAgent) Model() agent.Model { return c.sa.Model() }

func (c *coordinatorOverSessionAgent) UpdateModels(context.Context) error { return nil }

// GenerateTitle deliberately does not forward to c.sa.GenerateTitle: the
// real sessionAgent.Run already spawns its own detached title goroutine
// against the same model (see agent.go's hasUserTextMessage branch), so
// forwarding here would just trigger a second, redundant title call.
func (c *coordinatorOverSessionAgent) GenerateTitle(context.Context, string, string) {}

func (c *coordinatorOverSessionAgent) SetDelegationTools(tools.ThreadManager, tools.TaskManager) {}
func (c *coordinatorOverSessionAgent) DeliverTaskCompletion(context.Context, string, agent.TaskCompletion) {
}

func (c *coordinatorOverSessionAgent) RefreshSkills([]*skills.Skill, []*skills.Skill) {}

func (c *coordinatorOverSessionAgent) RegisterDelegationParent(sessionID string, parent agent.DelegationParent) {
	c.sa.RegisterDelegationParent(sessionID, parent)
}

func (c *coordinatorOverSessionAgent) SendToParent(ctx context.Context, sessionID, message string) error {
	return c.sa.SendToParent(ctx, sessionID, message)
}

// -- fake model --

// titleCallMaxTokens mirrors the fixed budget generateTitle sets for a
// non-reasoning model (see agent.go's generateTitle); it is the only
// marker available to tell the detached title call apart from the
// turn's own call, matching the technique internal/agent's own fakes use
// (see queued_runid_test.go's isTitleCall/titleStream).
const titleCallMaxTokens = 40

func isTitleCall(call fantasy.Call) bool {
	return call.MaxOutputTokens != nil && *call.MaxOutputTokens == titleCallMaxTokens
}

func titleStream() (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: "title"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "title"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

// blockingStreamModel streams a trivial finished response, but blocks
// the first non-title Stream call until gate is released, signalling
// entry via entered first. calls counts every non-title Stream
// invocation, so a test can assert a call never happened at all (the
// cancel-on-entry case) instead of only "didn't happen yet".
type blockingStreamModel struct {
	text    string
	gate    chan struct{}
	entered chan struct{}
	calls   atomic.Int64
}

func (m *blockingStreamModel) Provider() string { return "fake" }
func (m *blockingStreamModel) Model() string    { return "fake-model" }

func (m *blockingStreamModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: m.text}},
		FinishReason: fantasy.FinishReasonStop,
	}, nil
}

func (m *blockingStreamModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	if m.calls.Add(1) == 1 {
		close(m.entered)
		select {
		case <-m.gate:
		case <-ctx.Done():
		}
	}
	text := m.text
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *blockingStreamModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *blockingStreamModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func testCatalogModel() catwalk.Model {
	return catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}
}

// TestAppWorkspace_AgentRun_QueuedPromptVisibleWhileFirstActive_RealMachinery
// covers stage 4's cases 1 and 2 together: a second prompt sent to a
// session whose first turn is still streaming must be accepted without
// error and without waiting (the require.NoError right after the
// busy-session assertion below), and AgentQueuedPromptsList must report
// it, using the real internal/agent dispatch/queue machinery rather
// than a fake coordinator that would make the queue assertion vacuous.
// A separate fake-coordinator test for case 1 alone would be strictly
// weaker than this one and is not added.
//
// It would fail outright if AppWorkspace.AgentRun stopped dispatching
// through the coordinator's real accept/queue path (e.g. reverted to a
// direct synchronous Coordinator.Run call bypassing
// BeginAccepted/RunAccepted): the second call would then either block
// until the first finished (failing the accepted-without-waiting
// assertion) or never enqueue at all, leaving AgentQueuedPromptsList
// empty instead of []string{"second"}. It would also fail if
// AppWorkspace.AgentQueuedPromptsList stopped delegating to the
// coordinator's real QueuedPromptsList.
func TestAppWorkspace_AgentRun_QueuedPromptVisibleWhileFirstActive_RealMachinery(t *testing.T) {
	t.Parallel()

	sessions, messages := newRealSessionAgentEnv(t)
	sess, err := sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	model := &blockingStreamModel{text: "done", gate: make(chan struct{}), entered: make(chan struct{})}
	sa := agent.NewSessionAgent(agent.SessionAgentOptions{
		Model:    agent.Model{Model: model, CatalogCfg: testCatalogModel()},
		Sessions: sessions,
		Messages: messages,
	})
	// callReturned is closed after each RunAccepted invocation returns.
	// The first run's call blocks inside Stream and will not return
	// until the test closes model.gate, so the *second* close is the
	// deterministic, event-driven signal that the second run's
	// accept/queue handoff (BeginAccepted -> busy check -> enqueueCall
	// -> accept.Close) has actually landed — no polling needed.
	coord := &callReturnCoordinator{
		coordinatorOverSessionAgent: &coordinatorOverSessionAgent{sa: sa},
		callReturned:                make(chan struct{}),
	}

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.SetAgentCoordinatorForTest(coord)

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	require.NoError(t, aw.AgentRun(t.Context(), sess.ID, "first"))

	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first run never entered Stream")
	}
	require.True(t, aw.AgentIsSessionBusy(sess.ID), "first run must be active before the second is sent")

	require.NoError(t, aw.AgentRun(t.Context(), sess.ID, "second"))

	select {
	case <-coord.callReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("second run's accept/queue handoff never completed")
	}

	require.Equal(t, []string{"second"}, aw.AgentQueuedPromptsList(sess.ID))

	close(model.gate)

	waited := make(chan struct{})
	go func() {
		a.AgentDispatcher().Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not join both runs after the gate was released")
	}
}

// TestAppWorkspace_AgentRun_CancelBetweenAcceptAndActive_RealMachinery is
// case 3 of stage 4: a Cancel landing in the window between "accepted"
// and "entered sessionAgent.Run" must not be lost. The accept
// reservation (agent.AcceptedRun, taken by BeginAccepted inside
// AgentDispatcher.Send) is what makes this window observable to Cancel
// at all; see internal/agent/dispatch.go's cancelMark/acceptSeq
// machinery and agent.go's cancel-on-entry branch in sessionAgent.Run.
//
// It would fail if AppWorkspace.AgentRun (via AgentDispatcher.Send)
// stopped calling BeginAccepted before dispatching — Cancel would then
// find no accepted run for the session and record no mark, and the run
// would proceed to Stream when its gate opens, incrementing
// model.calls. It would also fail if AgentCancel stopped reaching the
// real coordinator (a no-op AgentCancel would leave the message
// unpersisted-canceled and let the model stream instead).
func TestAppWorkspace_AgentRun_CancelBetweenAcceptAndActive_RealMachinery(t *testing.T) {
	t.Parallel()

	sessions, messages := newRealSessionAgentEnv(t)
	sess, err := sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// gate starts closed: if the cancel-on-entry path is bypassed and
	// Stream is reached, the call proceeds instead of hanging, so the
	// test still terminates and the calls counter below still catches
	// the regression.
	closedGate := make(chan struct{})
	close(closedGate)
	model := &blockingStreamModel{text: "should never stream", gate: closedGate, entered: make(chan struct{}, 1)}
	sa := agent.NewSessionAgent(agent.SessionAgentOptions{
		Model:    agent.Model{Model: model, CatalogCfg: testCatalogModel()},
		Sessions: sessions,
		Messages: messages,
	})

	entered := make(chan struct{})
	runGate := make(chan struct{})
	coord := &gatedRunAcceptedCoordinator{
		coordinatorOverSessionAgent: &coordinatorOverSessionAgent{sa: sa},
		entered:                     entered,
		gate:                        runGate,
	}

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.SetAgentCoordinatorForTest(coord)

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	require.NoError(t, aw.AgentRun(t.Context(), sess.ID, "hi"))

	// BeginAccepted already ran synchronously inside AgentRun (via
	// AgentDispatcher.Send) before dispatch. The dispatched run has now
	// reached coord's gate but has not yet called the real RunAccepted,
	// so the accept handle is accepted but not active.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatched run never reached the gate")
	}

	// A cancel arriving now lands in the accepted-but-not-active window
	// and is only recorded because BeginAccepted incremented the accept
	// counter.
	aw.AgentCancel(sess.ID)

	// Release the gate so the real RunAccepted threads the handle into
	// sessionAgent.Run, which must drive cancel-on-entry.
	close(runGate)

	waited := make(chan struct{})
	go func() {
		a.AgentDispatcher().Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not join the canceled run")
	}

	require.Equal(t, int64(0), model.calls.Load(), "the model must never stream a run canceled before it became active")

	msgs, err := messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, message.Assistant, msgs[1].Role)
	require.Equal(t, message.FinishReasonCanceled, msgs[1].FinishReason())
}

// gatedRunAcceptedCoordinator parks RunAccepted before delegating to the
// wrapped coordinatorOverSessionAgent, so a cancel can be made to land
// in the accepted-but-not-yet-active window deterministically.
type gatedRunAcceptedCoordinator struct {
	*coordinatorOverSessionAgent
	entered chan struct{}
	gate    chan struct{}
}

func (c *gatedRunAcceptedCoordinator) RunAccepted(ctx context.Context, accept *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	close(c.entered)
	<-c.gate
	return c.coordinatorOverSessionAgent.RunAccepted(ctx, accept, sessionID, prompt, attachments...)
}

// callReturnCoordinator closes callReturned the first time any
// RunAccepted call returns. Used where exactly one of two concurrent
// calls is expected to return quickly (the busy-session enqueue path)
// while the other stays blocked until the test releases it later, so
// the first return observed is unambiguously the fast one — giving the
// test a deterministic, event-driven signal instead of a poll or sleep.
type callReturnCoordinator struct {
	*coordinatorOverSessionAgent
	once         sync.Once
	callReturned chan struct{}
}

func (c *callReturnCoordinator) RunAccepted(ctx context.Context, accept *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	res, err := c.coordinatorOverSessionAgent.RunAccepted(ctx, accept, sessionID, prompt, attachments...)
	c.once.Do(func() { close(c.callReturned) })
	return res, err
}
