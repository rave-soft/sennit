// Two frontends on one served workspace, over real gRPC, against the
// REAL permission.Service and question.Service (CLIENT-SERVER.md, PR
// 1.5): a permission or question request reaches every connected client,
// the first answer wins, the loser's own answer is a no-op, and both
// clients see the resolution notification. Also covers the tampered-copy
// regression guard for b7b095b47 (GrantPersistent must use the stored
// request's fields, never the caller's copy) over the wire, pending-
// request replay to a client that connects after the request was raised,
// and SetCurrentSession's plain pass-through (no per-client semantics; see
// PR 1.5's "Уточнено 2026-09-26" note).
package grpcws_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// realServiceWorkspace is a workspace.Workspace backed by a real
// permission.Service and question.Service, wired the same way
// app.NewForTest wires them (a live events broker fed by both services'
// request and notification brokers), rather than a full app.App bootstrap
// (database, LSP, MCP, agent coordinator) which none of these tests need.
// Every method this doesn't override panics on the embedded nil
// workspace.Workspace -- fine, since none of these tests call one.
type realServiceWorkspace struct {
	*wsrpctest.StubWorkspace
	app *app.App

	mu                     sync.Mutex
	setCurrentSessionCalls []string
}

func newRealServiceWorkspace(t *testing.T) *realServiceWorkspace {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := app.NewForTest(ctx)
	t.Cleanup(func() {
		a.ShutdownForTest()
		cancel()
	})
	return &realServiceWorkspace{StubWorkspace: &wsrpctest.StubWorkspace{}, app: a}
}

func (w *realServiceWorkspace) PermissionGrant(perm permission.PermissionRequest) (bool, error) {
	return w.app.Permissions().Grant(perm), nil
}

func (w *realServiceWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) (bool, error) {
	return w.app.Permissions().GrantPersistent(perm), nil
}

func (w *realServiceWorkspace) PermissionDeny(perm permission.PermissionRequest) (bool, error) {
	return w.app.Permissions().Deny(perm), nil
}

func (w *realServiceWorkspace) QuestionAnswer(batchID string, responses []question.Answer) (bool, error) {
	return w.app.Questions.Answer(batchID, responses), nil
}

func (w *realServiceWorkspace) QuestionCancel(batchID string) (bool, error) {
	return w.app.Questions.Cancel(batchID), nil
}

func (w *realServiceWorkspace) PendingPrompts(context.Context) (workspace.PendingPrompts, error) {
	var out workspace.PendingPrompts
	if req, ok := w.app.Permissions().ActiveRequest(); ok {
		out.Permissions = append(out.Permissions, req)
	}
	if req, ok := w.app.Questions.ActiveRequest(); ok {
		out.Questions = append(out.Questions, req)
	}
	return out, nil
}

// SetCurrentSession records every call it gets, from whichever client
// made it -- there is no per-client state to keep here (see PR 1.5's
// "Уточнено 2026-09-26" note: SetCurrentSession only ever reports to
// herdr, which in daemon mode is the connecting client's own concern, not
// this workspace's).
func (w *realServiceWorkspace) SetCurrentSession(_ context.Context, sessionID string) error {
	w.mu.Lock()
	w.setCurrentSessionCalls = append(w.setCurrentSessionCalls, sessionID)
	w.mu.Unlock()
	return nil
}

func (w *realServiceWorkspace) currentSessionCalls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.setCurrentSessionCalls...)
}

// SubscribeWith mirrors appws.AppWorkspace.SubscribeWith: an independently
// stoppable subscription against the App's own event broker, which is
// where both services' request/notification brokers are already fanned in
// (see app.NewForTest). No translateEvent step is needed here -- none of
// permission.PermissionRequest/PermissionNotification/question.Request/
// Notification is one of the types that step rewrites or drops.
func (w *realServiceWorkspace) SubscribeWith(send func(any)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		events := w.app.Events(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				send(ev.Payload)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func waitForPermissionRequest(t *testing.T, ch <-chan any, toolCallID string) permission.PermissionRequest {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case v := <-ch:
			if ev, ok := v.(pubsub.Event[permission.PermissionRequest]); ok && ev.Payload.ToolCallID == toolCallID {
				return ev.Payload
			}
		case <-deadline:
			t.Fatalf("permission request %q never arrived", toolCallID)
			return permission.PermissionRequest{}
		}
	}
}

func waitForPermissionNotification(t *testing.T, ch <-chan any, toolCallID string) permission.PermissionNotification {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case v := <-ch:
			if ev, ok := v.(pubsub.Event[permission.PermissionNotification]); ok &&
				ev.Payload.ToolCallID == toolCallID && (ev.Payload.Granted || ev.Payload.Denied) {
				return ev.Payload
			}
		case <-deadline:
			t.Fatalf("resolved permission notification for %q never arrived", toolCallID)
			return permission.PermissionNotification{}
		}
	}
}

func waitForQuestionRequest(t *testing.T, ch <-chan any, batchID string) question.Request {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case v := <-ch:
			if ev, ok := v.(pubsub.Event[question.Request]); ok && ev.Payload.ID == batchID {
				return ev.Payload
			}
		case <-deadline:
			t.Fatalf("question request %q never arrived", batchID)
			return question.Request{}
		}
	}
}

func waitForQuestionNotification(t *testing.T, ch <-chan any, batchID string) question.Notification {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case v := <-ch:
			if ev, ok := v.(pubsub.Event[question.Notification]); ok && ev.Payload.BatchID == batchID {
				return ev.Payload
			}
		case <-deadline:
			t.Fatalf("question notification for %q never arrived", batchID)
			return question.Notification{}
		}
	}
}

// noPermissionRequestArrives fails the test if a permission.PermissionRequest
// for toolCallID shows up on ch within window -- used by the pending-replay
// test to prove a client connecting after a request was already resolved
// does not get it replayed.
func noPermissionRequestArrives(t *testing.T, ch <-chan any, toolCallID string, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case v := <-ch:
			if ev, ok := v.(pubsub.Event[permission.PermissionRequest]); ok && ev.Payload.ToolCallID == toolCallID {
				t.Fatalf("received a stale permission request that was already resolved: %+v", ev.Payload)
			}
		case <-deadline:
			return
		}
	}
}

// TestMultiClient_PermissionGrantDenyRace is CLIENT-SERVER.md PR 1.5's own
// acceptance test: two clients, one permission request, both answer at
// once -- exactly one wins. Repeated 50 times with the winner picked at
// random so both orderings (grant-first and deny-first) are exercised.
func TestMultiClient_PermissionGrantDenyRace(t *testing.T) {
	t.Parallel()

	for i := range 50 {
		t.Run(fmt.Sprintf("iter-%d", i), func(t *testing.T) {
			t.Parallel()

			ws := newRealServiceWorkspace(t)
			srv, stopHub := grpcws.NewServer(ws)
			dialer := startServer(t, srv, stopHub)
			clientA := dialClient(t, dialer)
			clientB := dialClient(t, dialer)

			gotA := make(chan any, 128)
			stopA := clientA.SubscribeWith(func(v any) { gotA <- v })
			t.Cleanup(stopA)
			gotB := make(chan any, 128)
			stopB := clientB.SubscribeWith(func(v any) { gotB <- v })
			t.Cleanup(stopB)

			toolCallID := fmt.Sprintf("call-%d", i)
			reqCh := make(chan bool, 1)
			errCh := make(chan error, 1)
			go func() {
				granted, err := ws.app.Permissions().Request(context.Background(), permission.CreatePermissionRequest{
					SessionID:  "sess-race",
					ToolCallID: toolCallID,
					ToolName:   "bash",
					Action:     "execute",
					Path:       "/tmp/race",
				})
				reqCh <- granted
				errCh <- err
			}()

			permA := waitForPermissionRequest(t, gotA, toolCallID)
			permB := waitForPermissionRequest(t, gotB, toolCallID)
			require.Equal(t, permA.ID, permB.ID)

			var wg sync.WaitGroup
			var grantOK, denyOK bool
			var grantErr, denyErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
				grantOK, grantErr = clientA.PermissionGrant(permA)
			}()
			go func() {
				defer wg.Done()
				<-start
				time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
				denyOK, denyErr = clientB.PermissionDeny(permB)
			}()
			close(start)
			wg.Wait()

			require.NoError(t, grantErr)
			require.NoError(t, denyErr)
			require.NotEqual(t, grantOK, denyOK, "exactly one of grant/deny must resolve the request")

			granted := <-reqCh
			require.NoError(t, <-errCh)
			require.Equal(t, grantOK, granted, "the requester's own outcome must match whichever call actually won")

			notifA := waitForPermissionNotification(t, gotA, toolCallID)
			notifB := waitForPermissionNotification(t, gotB, toolCallID)
			require.Equal(t, notifA, notifB, "both clients must see the same resolution notification")
			require.Equal(t, grantOK, notifA.Granted)
			require.Equal(t, denyOK, notifA.Denied)
		})
	}
}

// TestMultiClient_GrantPersistentTamperedCopyUsesStoredFields is the wire
// regression guard for b7b095b47: GrantPersistent must record the
// persistent grant against the STORED request's fields, never the
// caller's copy. Client B answers with a copy whose ToolName/Params/Path
// were changed after it received the request (any client transporting a
// PermissionRequest over JSON could in principle do this, deliberately or
// through a decode bug) -- the grant it resolves must still be exactly
// what was actually asked.
func TestMultiClient_GrantPersistentTamperedCopyUsesStoredFields(t *testing.T) {
	t.Parallel()

	ws := newRealServiceWorkspace(t)
	srv, stopHub := grpcws.NewServer(ws)
	dialer := startServer(t, srv, stopHub)
	clientA := dialClient(t, dialer)
	clientB := dialClient(t, dialer)

	gotA := make(chan any, 128)
	stopA := clientA.SubscribeWith(func(v any) { gotA <- v })
	t.Cleanup(stopA)
	gotB := make(chan any, 128)
	stopB := clientB.SubscribeWith(func(v any) { gotB <- v })
	t.Cleanup(stopB)

	const sessionID = "sess-persist"
	origParams := map[string]any{"command": "ls"}
	toolCallID := "call-persist-1"
	reqCh := make(chan bool, 1)
	errCh := make(chan error, 1)
	go func() {
		granted, err := ws.app.Permissions().Request(context.Background(), permission.CreatePermissionRequest{
			SessionID:  sessionID,
			ToolCallID: toolCallID,
			ToolName:   "original-tool",
			Action:     "execute",
			Path:       "/tmp/original",
			Params:     origParams,
		})
		reqCh <- granted
		errCh <- err
	}()

	permA := waitForPermissionRequest(t, gotA, toolCallID)
	permB := waitForPermissionRequest(t, gotB, toolCallID)
	require.Equal(t, permA.ID, permB.ID)

	// Client B's copy names a different tool, path and params than the
	// request it is actually resolving (its own copy of permA/permB,
	// mutated locally -- exactly what a tampered or stale client would
	// send; "original-tool"/"tampered-tool" are both unregistered in
	// proto's permission-params registry, so both decode to a plain
	// map[string]any on the way over the wire regardless).
	tampered := permB
	tampered.ToolName = "tampered-tool"
	tampered.Path = "/tmp/evil"
	tampered.Params = map[string]any{"command": "rm -rf /"}

	ok, err := clientB.PermissionGrantPersistent(tampered)
	require.NoError(t, err)
	require.True(t, ok)

	granted := <-reqCh
	require.NoError(t, <-errCh)
	require.True(t, granted)

	waitForPermissionNotification(t, gotA, toolCallID)
	waitForPermissionNotification(t, gotB, toolCallID)

	// A second request matching the ORIGINAL fields auto-approves: the
	// persistent grant was recorded against the stored request, not the
	// tampered copy. Bounded with a short ctx timeout, not
	// context.Background(): if the grant landed under the tampered
	// fields instead, nothing will ever answer this one, and the point of
	// this test is to fail loudly on that, not hang the suite.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	granted2, err := ws.app.Permissions().Request(ctx2, permission.CreatePermissionRequest{
		SessionID:  sessionID,
		ToolCallID: "call-persist-2",
		ToolName:   "original-tool",
		Action:     "execute",
		Path:       "/tmp/original",
		Params:     origParams,
	})
	require.NoError(t, err)
	require.True(t, granted2, "the grant must be recorded for the original request's fields")

	// A request matching the TAMPERED fields is not auto-approved: no
	// grant was ever recorded for them. Bound the wait with a short ctx
	// timeout rather than blocking forever on an answer that will never
	// come.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	granted3, err := ws.app.Permissions().Request(ctx, permission.CreatePermissionRequest{
		SessionID:  sessionID,
		ToolCallID: "call-persist-3",
		ToolName:   "tampered-tool",
		Action:     "execute",
		Path:       "/tmp/evil",
		Params:     map[string]any{"command": "rm -rf /"},
	})
	require.False(t, granted3)
	require.Error(t, err, "the tampered fields must never have been granted, so this request should time out unanswered")
}

// TestMultiClient_QuestionAnswerCancelRace mirrors the permission race for
// question.Service: client A answers, client B cancels the same batch at
// once -- exactly one wins, and both see the resolution notification.
func TestMultiClient_QuestionAnswerCancelRace(t *testing.T) {
	t.Parallel()

	for i := range 10 {
		t.Run(fmt.Sprintf("iter-%d", i), func(t *testing.T) {
			t.Parallel()

			ws := newRealServiceWorkspace(t)
			srv, stopHub := grpcws.NewServer(ws)
			dialer := startServer(t, srv, stopHub)
			clientA := dialClient(t, dialer)
			clientB := dialClient(t, dialer)

			gotA := make(chan any, 128)
			stopA := clientA.SubscribeWith(func(v any) { gotA <- v })
			t.Cleanup(stopA)
			gotB := make(chan any, 128)
			stopB := clientB.SubscribeWith(func(v any) { gotB <- v })
			t.Cleanup(stopB)

			batchID := fmt.Sprintf("batch-%d", i)
			req := question.Request{
				ID:        batchID,
				SessionID: "sess-question-race",
				Questions: []question.Question{
					{ID: batchID + "-q1", Type: question.TypeYesNo, Text: "Proceed?", Description: "confirm the risky step"},
				},
			}

			answersCh := make(chan []question.Answer, 1)
			askErrCh := make(chan error, 1)
			go func() {
				answers, err := ws.app.Questions.Ask(context.Background(), req)
				answersCh <- answers
				askErrCh <- err
			}()

			gotReqA := waitForQuestionRequest(t, gotA, batchID)
			gotReqB := waitForQuestionRequest(t, gotB, batchID)
			require.Equal(t, gotReqA.ID, gotReqB.ID)

			answer := []question.Answer{{QuestionID: batchID + "-q1", Yes: boolPtr(true)}}

			var wg sync.WaitGroup
			var answerOK, cancelOK bool
			var answerErr, cancelErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
				answerOK, answerErr = clientA.QuestionAnswer(batchID, answer)
			}()
			go func() {
				defer wg.Done()
				<-start
				time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
				cancelOK, cancelErr = clientB.QuestionCancel(batchID)
			}()
			close(start)
			wg.Wait()

			require.NoError(t, answerErr)
			require.NoError(t, cancelErr)
			require.NotEqual(t, answerOK, cancelOK, "exactly one of answer/cancel must resolve the batch")

			notifA := waitForQuestionNotification(t, gotA, batchID)
			notifB := waitForQuestionNotification(t, gotB, batchID)
			require.Equal(t, notifA, notifB, "both clients must see the same resolution notification")

			if answerOK {
				require.NoError(t, <-askErrCh)
				got := <-answersCh
				require.Equal(t, answer, got)
			} else {
				require.ErrorIs(t, <-askErrCh, question.ErrCancelled)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// TestMultiClient_PendingPermissionReplaysToLateSubscriber covers a
// request raised while no client is connected: the client that connects
// afterward still receives it (the Snapshot-seeded replay client_pump.go
// implements), and answers it; a second client connecting after it was
// already resolved does not get it replayed.
func TestMultiClient_PendingPermissionReplaysToLateSubscriber(t *testing.T) {
	t.Parallel()

	ws := newRealServiceWorkspace(t)
	srv, stopHub := grpcws.NewServer(ws)
	dialer := startServer(t, srv, stopHub)

	const toolCallID = "call-pending"
	reqCh := make(chan bool, 1)
	errCh := make(chan error, 1)
	go func() {
		granted, err := ws.app.Permissions().Request(context.Background(), permission.CreatePermissionRequest{
			SessionID:  "sess-pending",
			ToolCallID: toolCallID,
			ToolName:   "pending-tool",
			Action:     "execute",
			Path:       "/tmp/pending",
		})
		reqCh <- granted
		errCh <- err
	}()

	require.Eventually(t, func() bool {
		req, ok := ws.app.Permissions().ActiveRequest()
		return ok && req.ToolCallID == toolCallID
	}, 2*time.Second, 5*time.Millisecond, "the request must be pending with no client connected yet")

	clientA := dialClient(t, dialer)
	gotA := make(chan any, 128)
	stopA := clientA.SubscribeWith(func(v any) { gotA <- v })
	t.Cleanup(stopA)

	permA := waitForPermissionRequest(t, gotA, toolCallID)
	ok, err := clientA.PermissionGrant(permA)
	require.NoError(t, err)
	require.True(t, ok)

	require.True(t, <-reqCh)
	require.NoError(t, <-errCh)

	clientB := dialClient(t, dialer)
	gotB := make(chan any, 128)
	stopB := clientB.SubscribeWith(func(v any) { gotB <- v })
	t.Cleanup(stopB)

	noPermissionRequestArrives(t, gotB, toolCallID, 200*time.Millisecond)
}

// TestMultiClient_SetCurrentSessionPassesThrough checks SetCurrentSession
// from two different clients both reach the served workspace: per PR
// 1.5's "Уточнено 2026-09-26" note there is no per-client routing to
// verify here, only that the call passes through unchanged from whichever
// client made it.
func TestMultiClient_SetCurrentSessionPassesThrough(t *testing.T) {
	t.Parallel()

	ws := newRealServiceWorkspace(t)
	srv, stopHub := grpcws.NewServer(ws)
	dialer := startServer(t, srv, stopHub)
	clientA := dialClient(t, dialer)
	clientB := dialClient(t, dialer)

	require.NoError(t, clientA.SetCurrentSession(context.Background(), "sess-a"))
	require.NoError(t, clientB.SetCurrentSession(context.Background(), "sess-b"))

	require.ElementsMatch(t, []string{"sess-a", "sess-b"}, ws.currentSessionCalls())
}
