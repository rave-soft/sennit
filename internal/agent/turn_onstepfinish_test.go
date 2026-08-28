package agent

import (
	"context"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/session"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
)

// getHookSessions wraps a sessionstore.Service and, from inside Get, invokes hook
// once the underlying row has been read but before Get returns to the
// caller. onStepFinish's own Get-then-SaveUsage span is exactly this window
// - wrapping it lets a test land a concurrent cost write inside that window
// deterministically, instead of racing real goroutines against a timing
// accident.
type getHookSessions struct {
	sessionstore.Service
	hook func(ctx context.Context, sess session.Session)
}

func (s *getHookSessions) Get(ctx context.Context, id string) (session.Session, error) {
	sess, err := s.Service.Get(ctx, id)
	if err != nil {
		return sess, err
	}
	if s.hook != nil {
		s.hook(ctx, sess)
	}
	return sess, nil
}

// TestOnStepFinish_ConcurrentCostWriteNotLost is the regression test for the
// same defect fixed in usage.go's summarize (see the SaveUsage comment
// there): onStepFinish used to Get the session, fold this step's cost into
// a local copy, then Save the whole row back - clobbering any cost a
// concurrent writer (e.g. a delegation finishing against the same session)
// landed in the Get-to-Save window. onStepFinish must instead fold its own
// delta with SaveUsage, the way summarize does, so both writers' costs
// survive.
func TestOnStepFinish_ConcurrentCostWriteNotLost(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	realSessions := sessionstore.NewService(q, conn, "/test/project")

	sess, err := realSessions.Create(ctx, "cost race")
	require.NoError(t, err)
	assistant, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)

	const concurrentDelta = 5.0
	sessions := &getHookSessions{
		Service: realSessions,
		hook: func(ctx context.Context, sess session.Session) {
			// Simulate another writer (e.g. a delegation finalizer)
			// landing its own cost delta between onStepFinish's Get
			// and its write-back.
			_, saveErr := realSessions.SaveUsage(ctx, sess, concurrentDelta)
			require.NoError(t, saveErr)
		},
	}

	// CostPer1MIn/Out chosen so 1 input + 1 output token costs exactly 2.0,
	// making the expected total easy to state.
	model := Model{CatalogCfg: catwalk.Model{CostPer1MIn: 1_000_000, CostPer1MOut: 1_000_000}}
	turn := &runTurn{
		agent:            &sessionAgent{sessions: sessions, messages: messages},
		ctx:              ctx,
		genCtx:           ctx,
		model:            model,
		currentAssistant: &assistant,
		currentSession:   sess,
		call:             SessionAgentCall{SessionID: sess.ID},
	}

	require.NoError(t, turn.onStepFinish(fantasy.StepResult{
		Response: fantasy.Response{
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		},
	}))

	const ownDelta = 2.0
	final, err := realSessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.InDelta(t, concurrentDelta+ownDelta, final.Cost, 1e-9,
		"onStepFinish's cost write must fold onto the concurrent writer's delta, not overwrite it")
}

// TestOnStepFinish_SessionLockRaceSafe exercises sessionLock the way
// PrepareStep and OnStepFinish actually use it - guarding stepMessages and
// currentSession - under -race, to guard the narrowed critical section:
// onStepFinish no longer holds sessionLock across its DB round trips or
// RotateThreshold, only around the stepMessages read and the currentSession
// write, so a concurrent writer of those same fields (standing in for the
// next step's PrepareStep) must still never race with it.
func TestOnStepFinish_SessionLockRaceSafe(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	messages := messagestore.NewService(q, messagestore.WithDebounce(0))
	sessions := sessionstore.NewService(q, conn, "/test/project")

	sess, err := sessions.Create(ctx, "lock race")
	require.NoError(t, err)
	assistant, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)

	turn := &runTurn{
		agent:            &sessionAgent{sessions: sessions, messages: messages},
		ctx:              ctx,
		genCtx:           ctx,
		currentAssistant: &assistant,
		currentSession:   sess,
		call:             SessionAgentCall{SessionID: sess.ID},
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Stands in for PrepareStep's write and stopOnContextWindow's
			// read, both taken under sessionLock in production.
			turn.sessionLock.Lock()
			turn.stepMessages = []fantasy.Message{{Role: "user"}}
			_ = turn.currentSession.ID
			turn.sessionLock.Unlock()
		}
	}()

	for range 20 {
		require.NoError(t, turn.onStepFinish(fantasy.StepResult{
			Response: fantasy.Response{FinishReason: fantasy.FinishReasonStop},
		}))
	}
	close(stop)
	wg.Wait()
}
