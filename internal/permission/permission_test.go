package permission

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPermissionService_AllowedCommands(t *testing.T) {
	tests := []struct {
		name         string
		allowedTools []string
		toolName     string
		action       string
		expected     bool
	}{
		{
			name:         "tool in allowlist",
			allowedTools: []string{"bash", "view"},
			toolName:     "bash",
			action:       "execute",
			expected:     true,
		},
		{
			name:         "tool:action in allowlist",
			allowedTools: []string{"bash:execute", "edit:create"},
			toolName:     "bash",
			action:       "execute",
			expected:     true,
		},
		{
			name:         "tool not in allowlist",
			allowedTools: []string{"view", "ls"},
			toolName:     "bash",
			action:       "execute",
			expected:     false,
		},
		{
			name:         "tool:action not in allowlist",
			allowedTools: []string{"bash:read", "edit:create"},
			toolName:     "bash",
			action:       "execute",
			expected:     false,
		},
		{
			name:         "empty allowlist",
			allowedTools: []string{},
			toolName:     "bash",
			action:       "execute",
			expected:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewPermissionService("/tmp", false, tt.allowedTools)

			// Create a channel to capture the permission request
			// Since we're testing the allowlist logic, we need to simulate the request
			ps := service.(*permissionService)

			// Test the allowlist logic directly
			commandKey := tt.toolName + ":" + tt.action
			allowed := false
			for _, cmd := range ps.allowedTools {
				if cmd == commandKey || cmd == tt.toolName {
					allowed = true
					break
				}
			}

			if allowed != tt.expected {
				t.Errorf("expected %v, got %v for tool %s action %s with allowlist %v",
					tt.expected, allowed, tt.toolName, tt.action, tt.allowedTools)
			}
		})
	}
}

func TestSkipRace(t *testing.T) {
	svc := NewPermissionService("/tmp", false, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		svc.SetSkipRequests(true)
	}()
	go func() {
		defer wg.Done()
		svc.SkipRequests()
	}()
	wg.Wait()
}

func TestPermissionService_SkipMode(t *testing.T) {
	service := NewPermissionService("/tmp", true, []string{})

	result, err := service.Request(t.Context(), CreatePermissionRequest{
		SessionID:   "test-session",
		ToolName:    "bash",
		Action:      "execute",
		Description: "test command",
		Path:        "/tmp",
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected permission to be granted in skip mode")
	}
}

func TestPermissionService_HookApproval(t *testing.T) {
	t.Parallel()

	t.Run("matching tool call ID short-circuits the prompt", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)

		ctx := WithHookApproval(t.Context(), "call-42")
		granted, err := service.Request(ctx, CreatePermissionRequest{
			SessionID:   "s1",
			ToolCallID:  "call-42",
			ToolName:    "bash",
			Action:      "execute",
			Description: "hook-approved command",
			Path:        "/tmp",
		})
		require.NoError(t, err)
		assert.True(t, granted, "hook-approved call should bypass the prompt")
	})

	t.Run("approval is scoped to the stamped tool call ID", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)

		// Stamp for call-42, ask for a different call ID — must not leak.
		ctx := WithHookApproval(t.Context(), "call-42")

		// Kick off a real request that will need a subscriber to resolve it.
		events := service.Subscribe(t.Context())
		var (
			wg      sync.WaitGroup
			granted bool
			err     error
		)
		wg.Go(func() {
			granted, err = service.Request(ctx, CreatePermissionRequest{
				SessionID:   "s1",
				ToolCallID:  "call-other",
				ToolName:    "bash",
				Action:      "execute",
				Description: "unrelated call",
				Path:        "/tmp",
			})
		})

		// Confirm the service published a real request (i.e. didn't bypass).
		event := <-events
		service.Deny(event.Payload)
		wg.Wait()
		require.NoError(t, err)
		assert.False(t, granted, "stamped approval must not apply to a different tool call")
	})

	t.Run("notifies subscribers that permission was granted", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)

		notifications := service.SubscribeNotifications(t.Context())

		ctx := WithHookApproval(t.Context(), "call-99")
		granted, err := service.Request(ctx, CreatePermissionRequest{
			SessionID:  "s1",
			ToolCallID: "call-99",
			ToolName:   "view",
			Action:     "read",
			Path:       "/tmp",
		})
		require.NoError(t, err)
		assert.True(t, granted)

		event := <-notifications
		assert.Equal(t, "call-99", event.Payload.ToolCallID)
		assert.True(t, event.Payload.Granted, "subscribers should see a granted notification")
	})
}

func TestPermissionService_SequentialProperties(t *testing.T) {
	t.Run("Sequential permission requests with persistent grants", func(t *testing.T) {
		service := NewPermissionService("/tmp", false, []string{})

		req1 := CreatePermissionRequest{
			SessionID:   "session1",
			ToolName:    "file_tool",
			Description: "Read file",
			Action:      "read",
			Params:      map[string]string{"file": "test.txt"},
			Path:        "/tmp/test.txt",
		}

		var result1 bool
		var wg sync.WaitGroup

		events := service.Subscribe(t.Context())

		wg.Go(func() {
			result1, _ = service.Request(t.Context(), req1)
		})

		var permissionReq PermissionRequest
		event := <-events

		permissionReq = event.Payload
		service.GrantPersistent(permissionReq)

		wg.Wait()
		assert.True(t, result1, "First request should be granted")

		// Second identical request should be automatically approved due to persistent permission
		req2 := CreatePermissionRequest{
			SessionID:   "session1",
			ToolName:    "file_tool",
			Description: "Read file again",
			Action:      "read",
			Params:      map[string]string{"file": "test.txt"},
			Path:        "/tmp/test.txt",
		}
		result2, err := service.Request(t.Context(), req2)
		require.NoError(t, err)
		assert.True(t, result2, "Second request should be auto-approved")
	})
	t.Run("Sequential requests with temporary grants", func(t *testing.T) {
		service := NewPermissionService("/tmp", false, []string{})

		req := CreatePermissionRequest{
			SessionID:   "session2",
			ToolName:    "file_tool",
			Description: "Write file",
			Action:      "write",
			Params:      map[string]string{"file": "test.txt"},
			Path:        "/tmp/test.txt",
		}

		events := service.Subscribe(t.Context())
		var result1 bool
		var wg sync.WaitGroup

		wg.Go(func() {
			result1, _ = service.Request(t.Context(), req)
		})

		var permissionReq PermissionRequest
		event := <-events
		permissionReq = event.Payload

		service.Grant(permissionReq)
		wg.Wait()
		assert.True(t, result1, "First request should be granted")

		var result2 bool

		wg.Go(func() {
			result2, _ = service.Request(t.Context(), req)
		})

		event = <-events
		permissionReq = event.Payload
		service.Deny(permissionReq)
		wg.Wait()
		assert.False(t, result2, "Second request should be denied")
	})
	t.Run("Concurrent requests with different outcomes", func(t *testing.T) {
		service := NewPermissionService("/tmp", false, []string{})

		events := service.Subscribe(t.Context())

		var wg sync.WaitGroup
		results := make([]bool, 3)

		requests := []CreatePermissionRequest{
			{
				SessionID:   "concurrent1",
				ToolName:    "tool1",
				Action:      "action1",
				Path:        "/tmp/file1.txt",
				Description: "First concurrent request",
			},
			{
				SessionID:   "concurrent2",
				ToolName:    "tool2",
				Action:      "action2",
				Path:        "/tmp/file2.txt",
				Description: "Second concurrent request",
			},
			{
				SessionID:   "concurrent3",
				ToolName:    "tool3",
				Action:      "action3",
				Path:        "/tmp/file3.txt",
				Description: "Third concurrent request",
			},
		}

		for i, req := range requests {
			wg.Go(func() {
				result, _ := service.Request(t.Context(), req)
				results[i] = result
			})
		}

		for range 3 {
			event := <-events
			switch event.Payload.ToolName {
			case "tool1":
				service.Grant(event.Payload)
			case "tool2":
				service.GrantPersistent(event.Payload)
			case "tool3":
				service.Deny(event.Payload)
			}
		}
		wg.Wait()
		grantedCount := 0
		for _, result := range results {
			if result {
				grantedCount++
			}
		}

		assert.Equal(t, 2, grantedCount, "Should have 2 granted and 1 denied")
		secondReq := requests[1]
		secondReq.Description = "Repeat of second request"
		result, err := service.Request(t.Context(), secondReq)
		require.NoError(t, err)
		assert.True(t, result, "Repeated request should be auto-approved due to persistent permission")
	})
}

// TestPermissionService_ResolveIdempotency covers the multi-subscriber
// resolve guarantees: exactly one notification per resolution, racing
// callers see "already resolved", and stray Grant/Deny calls for unknown
// IDs are safe no-ops.
func TestPermissionService_ResolveIdempotency(t *testing.T) {
	t.Parallel()

	t.Run("concurrent grants resolve exactly once", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)

		events := service.Subscribe(t.Context())
		notifications := service.SubscribeNotifications(t.Context())

		req := CreatePermissionRequest{
			SessionID:  "race-session",
			ToolCallID: "race-call",
			ToolName:   "tool",
			Action:     "act",
			Path:       "/tmp/race",
		}

		var (
			wg         sync.WaitGroup
			granted    bool
			requestErr error
		)
		wg.Go(func() {
			granted, requestErr = service.Request(t.Context(), req)
		})

		// Wait for the request to be published so we have a real
		// PermissionRequest (with its server-side ID) to race on.
		var pending PermissionRequest
		select {
		case ev := <-events:
			pending = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("permission request was never published")
		}

		// Drain the initial "request opened" notification (Granted ==
		// false && Denied == false) so the next read is the resolution
		// itself.
		select {
		case ev := <-notifications:
			require.False(t, ev.Payload.Granted, "initial notification must not be granted")
			require.False(t, ev.Payload.Denied, "initial notification must not be denied")
		case <-time.After(2 * time.Second):
			t.Fatal("initial notification was never published")
		}

		// Race two grants from two goroutines.
		var (
			resolvedCount atomic.Int32
			start         = make(chan struct{})
			racers        sync.WaitGroup
		)
		for range 2 {
			racers.Go(func() {
				<-start
				if service.Grant(pending) {
					resolvedCount.Add(1)
				}
			})
		}
		close(start)
		racers.Wait()

		// Original Request must return granted exactly once.
		wg.Wait()
		require.NoError(t, requestErr)
		assert.True(t, granted, "request should observe its grant")

		// Exactly one of the two grants resolved the request.
		assert.Equal(t, int32(1), resolvedCount.Load(),
			"exactly one Grant should report it resolved the request")

		// Exactly one resolution notification, and no further ones.
		select {
		case ev := <-notifications:
			assert.True(t, ev.Payload.Granted, "resolution notification should be granted")
			assert.Equal(t, "race-call", ev.Payload.ToolCallID)
		case <-time.After(2 * time.Second):
			t.Fatal("resolution notification was never published")
		}
		select {
		case ev := <-notifications:
			t.Fatalf("unexpected duplicate notification: %+v", ev.Payload)
		case <-time.After(50 * time.Millisecond):
			// good: no duplicate.
		}

		// pendingRequests must be empty: no goroutine is left blocked
		// on a send, and a future Grant for the same ID is a no-op.
		ps := service.(*permissionService)
		assert.Equal(t, 0, ps.pendingRequests.Len(),
			"pendingRequests must be empty after resolution")

		assert.False(t, service.Grant(pending),
			"a third Grant should report already-resolved")
	})

	t.Run("grant after deny is a no-op", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)

		events := service.Subscribe(t.Context())
		notifications := service.SubscribeNotifications(t.Context())

		req := CreatePermissionRequest{
			SessionID:  "deny-first",
			ToolCallID: "df-call",
			ToolName:   "tool",
			Action:     "act",
			Path:       "/tmp/df",
		}

		var (
			wg         sync.WaitGroup
			granted    bool
			requestErr error
		)
		wg.Go(func() {
			granted, requestErr = service.Request(t.Context(), req)
		})

		var pending PermissionRequest
		select {
		case ev := <-events:
			pending = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("permission request was never published")
		}

		// Drain the initial neither-granted-nor-denied notification.
		<-notifications

		assert.True(t, service.Deny(pending), "Deny should resolve the request")
		wg.Wait()
		require.NoError(t, requestErr)
		assert.False(t, granted, "request should observe denial")

		// A follow-up Grant must be a no-op and must not flip the
		// outcome or publish anything new.
		assert.False(t, service.Grant(pending),
			"Grant after Deny should report already-resolved")

		select {
		case ev := <-notifications:
			// The first resolution notification (denial) is expected;
			// anything after that is a bug.
			require.True(t, ev.Payload.Denied,
				"the only post-initial notification must be the denial")
		case <-time.After(2 * time.Second):
			t.Fatal("denial notification was never published")
		}
		select {
		case ev := <-notifications:
			t.Fatalf("Grant after Deny must not publish: %+v", ev.Payload)
		case <-time.After(50 * time.Millisecond):
			// good.
		}
	})

	t.Run("losing GrantPersistent does not record session permission", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)

		events := service.Subscribe(t.Context())
		notifications := service.SubscribeNotifications(t.Context())

		req := CreatePermissionRequest{
			SessionID:  "race-persist",
			ToolCallID: "rp-call",
			ToolName:   "tool",
			Action:     "act",
			Path:       "/tmp/rp",
		}

		var (
			wg         sync.WaitGroup
			granted    bool
			requestErr error
		)
		wg.Go(func() {
			granted, requestErr = service.Request(t.Context(), req)
		})

		// Wait for the request to be published so we have the real
		// pending PermissionRequest to race on.
		var pending PermissionRequest
		select {
		case ev := <-events:
			pending = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("permission request was never published")
		}

		// Drain the initial neither-granted-nor-denied notification.
		<-notifications

		// Deny wins, then a competing GrantPersistent loses.
		assert.True(t, service.Deny(pending), "Deny should resolve the request")
		assert.False(t, service.GrantPersistent(pending),
			"GrantPersistent after Deny should report already-resolved")

		wg.Wait()
		require.NoError(t, requestErr)
		assert.False(t, granted, "request should observe denial")

		// The losing GrantPersistent must not have inserted an
		// auto-approve entry. Issue a matching follow-up request and
		// confirm the service still publishes a pending request (i.e.
		// not auto-approved). We then Deny it to drain the goroutine.
		var (
			wg2         sync.WaitGroup
			granted2    bool
			requestErr2 error
		)
		wg2.Go(func() {
			granted2, requestErr2 = service.Request(t.Context(), req)
		})

		select {
		case ev := <-events:
			assert.Equal(t, pending.SessionID, ev.Payload.SessionID)
			service.Deny(ev.Payload)
		case <-time.After(2 * time.Second):
			t.Fatal("follow-up request was auto-approved; persistent grant leaked")
		}

		wg2.Wait()
		require.NoError(t, requestErr2)
		assert.False(t, granted2, "follow-up request should be denied, not auto-approved")
	})

	t.Run("grant for unknown id is a safe no-op", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)

		notifications := service.SubscribeNotifications(t.Context())

		bogus := PermissionRequest{
			ID:         "does-not-exist",
			ToolCallID: "ghost",
			ToolName:   "tool",
			Action:     "act",
			Path:       "/tmp/ghost",
		}

		assert.NotPanics(t, func() {
			assert.False(t, service.Grant(bogus),
				"Grant for unknown ID should report already-resolved")
			assert.False(t, service.GrantPersistent(bogus),
				"GrantPersistent for unknown ID should report already-resolved")
			assert.False(t, service.Deny(bogus),
				"Deny for unknown ID should report already-resolved")
		})

		select {
		case ev := <-notifications:
			t.Fatalf("unknown-ID resolution must not publish: %+v", ev.Payload)
		case <-time.After(50 * time.Millisecond):
			// good: no notification.
		}
	})
}

// queueLen reports how many requests are currently waiting behind the
// one shown to the UI. It reaches into the service's private dispatch
// state, which is fine since this test file lives in package permission.
func queueLen(t *testing.T, svc Service) int {
	t.Helper()
	ps := svc.(*permissionService)
	ps.dialogMu.Lock()
	defer ps.dialogMu.Unlock()
	return len(ps.queue)
}

// queueOrder returns the ToolCallID of every request currently queued, in
// order, for asserting relative ordering without racing dispatch.
func queueOrder(t *testing.T, svc Service) []string {
	t.Helper()
	ps := svc.(*permissionService)
	ps.dialogMu.Lock()
	defer ps.dialogMu.Unlock()
	ids := make([]string, len(ps.queue))
	for i, p := range ps.queue {
		ids[i] = p.ToolCallID
	}
	return ids
}

// TestPermissionService_QueuedDispatch covers the fix for the
// requestMu-held-across-the-wait bug: Request must not block *posting* a second request behind a
// first one still awaiting a user response, but the UI must still only
// ever see one PermissionRequest event outstanding at a time.
func TestPermissionService_QueuedDispatch(t *testing.T) {
	t.Parallel()

	t.Run("second request is queued, not blocked, and published only after the first resolves", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		req1 := CreatePermissionRequest{SessionID: "s1", ToolCallID: "call-1", ToolName: "bash", Action: "execute", Path: "/tmp"}
		req2 := CreatePermissionRequest{SessionID: "s2", ToolCallID: "call-2", ToolName: "bash", Action: "execute", Path: "/tmp"}

		var wg sync.WaitGroup
		var granted1, granted2 bool
		wg.Go(func() {
			granted1, _ = service.Request(t.Context(), req1)
		})

		var pending1 PermissionRequest
		select {
		case ev := <-events:
			pending1 = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("first request was never published")
		}

		// Fire the second request while the first is still pending a
		// user response. This call must return from enqueueing without
		// waiting on the first to resolve — only the goroutine blocks
		// on its own respCh, not the caller of Request.
		done2 := make(chan struct{})
		wg.Go(func() {
			granted2, _ = service.Request(t.Context(), req2)
			close(done2)
		})

		// The second request must land in the queue rather than being
		// published: the UI must never see two outstanding requests.
		require.Eventually(t, func() bool {
			return queueLen(t, service) == 1
		}, 2*time.Second, 5*time.Millisecond, "second request should be queued")

		select {
		case ev := <-events:
			t.Fatalf("second request published before the first resolved: %+v", ev.Payload)
		case <-time.After(100 * time.Millisecond):
			// good: still queued.
		}

		service.Grant(pending1)

		var pending2 PermissionRequest
		select {
		case ev := <-events:
			pending2 = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("second request was never published after the first resolved")
		}
		assert.Equal(t, "call-2", pending2.ToolCallID)

		service.Deny(pending2)
		<-done2
		wg.Wait()

		assert.True(t, granted1)
		assert.False(t, granted2)
	})

	t.Run("canceling a queued request removes it without ever showing it", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		req1 := CreatePermissionRequest{SessionID: "s1", ToolCallID: "call-1", ToolName: "bash", Action: "execute", Path: "/tmp"}
		req2 := CreatePermissionRequest{SessionID: "s2", ToolCallID: "call-2", ToolName: "bash", Action: "execute", Path: "/tmp"}

		var wg sync.WaitGroup
		var granted1 bool
		wg.Go(func() {
			granted1, _ = service.Request(t.Context(), req1)
		})

		var pending1 PermissionRequest
		select {
		case ev := <-events:
			pending1 = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("first request was never published")
		}

		ctx2, cancel2 := context.WithCancel(t.Context())
		var granted2 bool
		var err2 error
		wg.Go(func() {
			granted2, err2 = service.Request(ctx2, req2)
		})

		require.Eventually(t, func() bool {
			return queueLen(t, service) == 1
		}, 2*time.Second, 5*time.Millisecond, "second request should be queued")

		cancel2()

		// Wait for req2's cancellation to be fully processed before
		// resolving req1, so we can assert the queue was drained by the
		// cancellation itself rather than by req1's resolution.
		require.Eventually(t, func() bool {
			return queueLen(t, service) == 0
		}, 2*time.Second, 5*time.Millisecond, "canceled queued request must be removed")

		select {
		case ev := <-events:
			t.Fatalf("canceled queued request must never be published: %+v", ev.Payload)
		case <-time.After(50 * time.Millisecond):
			// good: never shown.
		}

		service.Grant(pending1)
		wg.Wait()

		assert.ErrorIs(t, err2, context.Canceled)
		assert.False(t, granted2)
		assert.True(t, granted1)
	})

	t.Run("canceling the current request publishes the next queued one", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		req1 := CreatePermissionRequest{SessionID: "s1", ToolCallID: "call-1", ToolName: "bash", Action: "execute", Path: "/tmp"}
		req2 := CreatePermissionRequest{SessionID: "s2", ToolCallID: "call-2", ToolName: "bash", Action: "execute", Path: "/tmp"}

		ctx1, cancel1 := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		var granted1 bool
		var err1 error
		wg.Go(func() {
			granted1, err1 = service.Request(ctx1, req1)
		})

		select {
		case <-events:
		case <-time.After(2 * time.Second):
			t.Fatal("first request was never published")
		}

		var granted2 bool
		wg.Go(func() {
			granted2, _ = service.Request(t.Context(), req2)
		})

		require.Eventually(t, func() bool {
			return queueLen(t, service) == 1
		}, 2*time.Second, 5*time.Millisecond, "second request should be queued")

		cancel1()

		var pending2 PermissionRequest
		select {
		case ev := <-events:
			pending2 = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("second request was never published after the current one was canceled")
		}
		assert.Equal(t, "call-2", pending2.ToolCallID)

		service.Grant(pending2)
		wg.Wait()

		assert.ErrorIs(t, err1, context.Canceled)
		assert.False(t, granted1)
		assert.True(t, granted2)
	})
}

// TestPermissionService_DelegationAttribution covers WithDelegation: a
// request made under a delegation ctx is published carrying that ref, one
// made without carries the zero ref (today's only value), and neither
// case's grant/deny behavior changes.
func TestPermissionService_DelegationAttribution(t *testing.T) {
	t.Parallel()

	t.Run("request under a delegation ctx is published carrying the ref", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		ref := DelegationRef{ID: "task-1", Name: "task-abc", Kind: "task"}
		ctx := WithDelegation(t.Context(), ref)

		var (
			wg      sync.WaitGroup
			granted bool
			err     error
		)
		wg.Go(func() {
			granted, err = service.Request(ctx, CreatePermissionRequest{
				SessionID:  "task-session",
				ToolCallID: "call-deleg",
				ToolName:   "bash",
				Action:     "execute",
				Path:       "/tmp",
			})
		})

		var pending PermissionRequest
		select {
		case ev := <-events:
			pending = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("request was never published")
		}
		assert.Equal(t, ref, pending.Delegation, "published request should carry the delegation ref")

		service.Grant(pending)
		wg.Wait()
		require.NoError(t, err)
		assert.True(t, granted, "delegated requests are gated exactly like any other: grant still works")
	})

	t.Run("request without a delegation ctx is published with the zero ref", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		req := CreatePermissionRequest{
			SessionID:   "visible-session",
			ToolCallID:  "call-visible",
			ToolName:    "bash",
			Action:      "execute",
			Description: "the visible turn's own command",
			Params:      map[string]string{"cmd": "ls"},
			Path:        "/tmp",
		}

		var (
			wg      sync.WaitGroup
			granted bool
			err     error
		)
		wg.Go(func() {
			granted, err = service.Request(t.Context(), req)
		})

		var pending PermissionRequest
		select {
		case ev := <-events:
			pending = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("request was never published")
		}
		assert.Equal(t, DelegationRef{}, pending.Delegation, "an absent delegation ctx means the zero ref")
		// Every other field is exactly what Request always produced —
		// nothing about the rest of the published request changed.
		assert.Equal(t, req.SessionID, pending.SessionID)
		assert.Equal(t, req.ToolCallID, pending.ToolCallID)
		assert.Equal(t, req.ToolName, pending.ToolName)
		assert.Equal(t, req.Action, pending.Action)
		assert.Equal(t, req.Description, pending.Description)
		assert.Equal(t, req.Params, pending.Params)
		assert.Equal(t, req.Path, pending.Path)
		assert.NotEmpty(t, pending.ID)

		service.Deny(pending)
		wg.Wait()
		require.NoError(t, err)
		assert.False(t, granted, "denial still works exactly as before")
	})

	t.Run("ref survives to the subscriber path once a queued request is dispatched", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		ref := DelegationRef{ID: "task-2", Name: "task-def", Kind: "task"}
		delegatedCtx := WithDelegation(t.Context(), ref)

		req1 := CreatePermissionRequest{SessionID: "s1", ToolCallID: "call-1", ToolName: "bash", Action: "execute", Path: "/tmp"}
		req2 := CreatePermissionRequest{SessionID: "s2", ToolCallID: "call-2", ToolName: "bash", Action: "execute", Path: "/tmp"}

		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = service.Request(t.Context(), req1)
		})

		var pending1 PermissionRequest
		select {
		case ev := <-events:
			pending1 = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("first request was never published")
		}

		// The second, delegated request is queued behind the first (see
		// TestPermissionService_QueuedDispatch) rather than published
		// immediately — it reaches Subscribe through dispatchNext's
		// republish, a different code path than the one the first two
		// subtests exercised.
		wg.Go(func() {
			_, _ = service.Request(delegatedCtx, req2)
		})
		require.Eventually(t, func() bool {
			return queueLen(t, service) == 1
		}, 2*time.Second, 5*time.Millisecond, "second request should be queued")

		service.Grant(pending1)

		var pending2 PermissionRequest
		select {
		case ev := <-events:
			pending2 = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("second request was never published after the first resolved")
		}
		assert.Equal(t, ref, pending2.Delegation, "the ref must survive dispatchNext's republish")

		service.Deny(pending2)
		wg.Wait()
	})
}

// TestPermissionService_QueuePriority covers the fix that keeps a user
// continuing their own conversation from being made to answer for queued
// background delegations first: a foreground request (no delegation ref)
// is dispatched ahead of every queued background one, each class stays
// FIFO among itself, and whatever is already on screen is never preempted
// by what arrives behind it.
func TestPermissionService_QueuePriority(t *testing.T) {
	t.Parallel()

	t.Run("a foreground request queued behind background ones is dispatched first, and each class stays FIFO", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		holder := CreatePermissionRequest{SessionID: "s0", ToolCallID: "holder", ToolName: "bash", Action: "execute", Path: "/tmp"}
		bg1 := CreatePermissionRequest{SessionID: "s1", ToolCallID: "bg-1", ToolName: "bash", Action: "execute", Path: "/tmp"}
		bg2 := CreatePermissionRequest{SessionID: "s2", ToolCallID: "bg-2", ToolName: "bash", Action: "execute", Path: "/tmp"}
		fg := CreatePermissionRequest{SessionID: "s3", ToolCallID: "fg", ToolName: "bash", Action: "execute", Path: "/tmp"}

		var wg sync.WaitGroup
		wg.Go(func() { _, _ = service.Request(t.Context(), holder) })

		var current PermissionRequest
		select {
		case ev := <-events:
			current = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("holder request was never published")
		}

		// Queue two background requests, then a foreground one behind them.
		wg.Go(func() {
			_, _ = service.Request(WithDelegation(t.Context(), DelegationRef{ID: "t1", Name: "task-t1", Kind: "task"}), bg1)
		})
		require.Eventually(t, func() bool { return queueLen(t, service) == 1 }, 2*time.Second, 5*time.Millisecond)
		wg.Go(func() {
			_, _ = service.Request(WithDelegation(t.Context(), DelegationRef{ID: "t2", Name: "task-t2", Kind: "task"}), bg2)
		})
		require.Eventually(t, func() bool { return queueLen(t, service) == 2 }, 2*time.Second, 5*time.Millisecond)
		wg.Go(func() { _, _ = service.Request(t.Context(), fg) })
		require.Eventually(t, func() bool { return queueLen(t, service) == 3 }, 2*time.Second, 5*time.Millisecond)

		// The foreground request jumped ahead of both background ones,
		// which kept their own relative order.
		assert.Equal(t, []string{"fg", "bg-1", "bg-2"}, queueOrder(t, service))

		service.Grant(current)
		for _, want := range []string{"fg", "bg-1", "bg-2"} {
			var dispatched PermissionRequest
			select {
			case ev := <-events:
				dispatched = ev.Payload
			case <-time.After(2 * time.Second):
				t.Fatalf("%s was never published", want)
			}
			assert.Equal(t, want, dispatched.ToolCallID)
			service.Grant(dispatched)
		}
		wg.Wait()
	})

	t.Run("a background request on screen is never preempted by a foreground one arriving", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		bg := CreatePermissionRequest{SessionID: "s1", ToolCallID: "bg-current", ToolName: "bash", Action: "execute", Path: "/tmp"}
		fg := CreatePermissionRequest{SessionID: "s2", ToolCallID: "fg-late", ToolName: "bash", Action: "execute", Path: "/tmp"}

		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = service.Request(WithDelegation(t.Context(), DelegationRef{ID: "t1", Name: "task-t1", Kind: "task"}), bg)
		})

		var current PermissionRequest
		select {
		case ev := <-events:
			current = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("background request was never published")
		}
		require.Equal(t, "bg-current", current.ToolCallID)

		wg.Go(func() { _, _ = service.Request(t.Context(), fg) })
		require.Eventually(t, func() bool { return queueLen(t, service) == 1 }, 2*time.Second, 5*time.Millisecond,
			"foreground request should queue behind the current one")

		// Nothing new is published: the background request stays on
		// screen, unpreempted, until it resolves — regardless of what a
		// higher-priority class has waiting behind it.
		select {
		case ev := <-events:
			t.Fatalf("current request was preempted: %+v", ev.Payload)
		case <-time.After(100 * time.Millisecond):
			// good.
		}

		service.Grant(current)
		var dispatched PermissionRequest
		select {
		case ev := <-events:
			dispatched = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("foreground request was never published after the background one resolved")
		}
		assert.Equal(t, "fg-late", dispatched.ToolCallID)

		service.Grant(dispatched)
		wg.Wait()
	})

	t.Run("stable FIFO within each class when interleaved", func(t *testing.T) {
		t.Parallel()
		service := NewPermissionService("/tmp", false, nil)
		events := service.Subscribe(t.Context())

		holder := CreatePermissionRequest{SessionID: "s0", ToolCallID: "holder", ToolName: "bash", Action: "execute", Path: "/tmp"}
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = service.Request(t.Context(), holder) })

		var current PermissionRequest
		select {
		case ev := <-events:
			current = ev.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("holder request was never published")
		}

		// Interleave bg-1, fg-1, bg-2, fg-2 behind the holder. Expected
		// resulting order: fg-1, fg-2 (FIFO among foreground, both ahead
		// of background), then bg-1, bg-2 (FIFO among background).
		reqs := []struct {
			id  string
			ref DelegationRef
		}{
			{"bg-1", DelegationRef{ID: "t1", Kind: "task"}},
			{"fg-1", DelegationRef{}},
			{"bg-2", DelegationRef{ID: "t2", Kind: "task"}},
			{"fg-2", DelegationRef{}},
		}
		for i, r := range reqs {
			ctx := t.Context()
			if r.ref != (DelegationRef{}) {
				ctx = WithDelegation(ctx, r.ref)
			}
			req := CreatePermissionRequest{SessionID: "s", ToolCallID: r.id, ToolName: "bash", Action: "execute", Path: "/tmp"}
			wg.Go(func() { _, _ = service.Request(ctx, req) })
			want := i + 1
			require.Eventually(t, func() bool { return queueLen(t, service) == want }, 2*time.Second, 5*time.Millisecond)
		}

		assert.Equal(t, []string{"fg-1", "fg-2", "bg-1", "bg-2"}, queueOrder(t, service))

		service.Grant(current)
		for _, want := range []string{"fg-1", "fg-2", "bg-1", "bg-2"} {
			var dispatched PermissionRequest
			select {
			case ev := <-events:
				dispatched = ev.Payload
			case <-time.After(2 * time.Second):
				t.Fatalf("%s was never published", want)
			}
			assert.Equal(t, want, dispatched.ToolCallID)
			service.Grant(dispatched)
		}
		wg.Wait()
	})
}

// TestPermissionService_RequestSurvivesASlowSubscriber is the regression
// test for a hang with no visible cause. A permission request used to be
// announced with the broker's lossy Publish: if the subscriber's buffer
// was momentarily full the event was dropped on the spot, while the
// caller stayed blocked in Request waiting for an answer that could no
// longer come.
//
// Buffers are per-subscriber, so the subscriber that loses the event is
// the one that is behind — the TUI, exactly when it is struggling. Every
// other event in this system is corrected by the next one; a lost request
// is corrected by nothing, and surfaces as a tool call that stopped for
// no reason.
//
// The buffer here is shrunk to one and pre-filled so "behind" is a
// certainty rather than a race. The subscriber then drains, as a
// recovering UI would: bounded-blocking delivery waits out that moment,
// the lossy publish it replaced would already have thrown the event away.
func TestPermissionService_RequestSurvivesASlowSubscriber(t *testing.T) {
	t.Parallel()

	svc := NewPermissionService(t.TempDir(), false, nil)
	broker := pubsub.NewBrokerWithOptions[PermissionRequest](1)
	// Widened so the test measures the semantics, not the clock: the
	// production window is 50ms, which would make "is the subscriber
	// still behind when the publish happens" a race with the drain below.
	broker.SetMustDeliverTimeout(10 * time.Second)
	svc.(*permissionService).Broker = broker

	watching := svc.Subscribe(t.Context())
	// Fill the subscriber's only slot, putting it behind.
	svc.(*permissionService).Publish(pubsub.CreatedEvent, PermissionRequest{ID: "filler"})

	go func() {
		_, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID:  "sess-1",
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
			Path:       t.TempDir(),
		})
	}()

	// Let the publish actually happen while the subscriber is still
	// behind — ActiveRequest flips just before it, so this waits for the
	// publish to be underway rather than guessing.
	require.Eventually(t, func() bool {
		_, ok := svc.ActiveRequest()
		return ok
	}, 5*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	// Catch up, the way a UI that was momentarily busy would.
	filler := <-watching
	require.Equal(t, "filler", filler.Payload.ID)

	select {
	case ev := <-watching:
		require.Equal(t, "bash", ev.Payload.ToolName,
			"the request must survive a subscriber that was briefly behind")
	case <-time.After(5 * time.Second):
		t.Fatal("the permission request never reached the subscriber")
	}
}

// TestPermissionService_ActiveRequestRecoversAMissedPublish covers the
// other half: bounded-blocking delivery still permits a drop after its
// timeout, and a subscriber that attaches later missed the publish
// outright — a subscription only carries future events. Without a way to
// ask what is currently pending, either case is an unrecoverable hang.
func TestPermissionService_ActiveRequestRecoversAMissedPublish(t *testing.T) {
	t.Parallel()

	svc := NewPermissionService(t.TempDir(), false, nil)

	_, ok := svc.ActiveRequest()
	require.False(t, ok, "nothing is pending yet")

	granted := make(chan bool, 1)
	go func() {
		ok, _ := svc.Request(t.Context(), CreatePermissionRequest{
			SessionID:  "sess-1",
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
			Path:       t.TempDir(),
		})
		granted <- ok
	}()

	// Nobody was subscribed when this was published: exactly the case
	// that used to be unrecoverable.
	var pending PermissionRequest
	require.Eventually(t, func() bool {
		var ok bool
		pending, ok = svc.ActiveRequest()
		return ok
	}, 5*time.Second, 5*time.Millisecond,
		"a pending request must remain discoverable after its publish has gone by")

	require.Equal(t, "bash", pending.ToolName)
	require.NotEmpty(t, pending.ID, "the recovered request must be answerable, so it needs its id")

	require.True(t, svc.Grant(pending), "the recovered request must be the real one")
	select {
	case ok := <-granted:
		require.True(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("granting the recovered request did not release its caller")
	}

	_, ok = svc.ActiveRequest()
	require.False(t, ok, "a resolved request must stop being reported as pending")
}

// TestPermissionService_ActiveRequestFollowsTheQueue: only the request
// actually on screen is recoverable, since only it has been published.
// Reporting a queued one would invite an answer to a prompt the user was
// never shown.
func TestPermissionService_ActiveRequestFollowsTheQueue(t *testing.T) {
	t.Parallel()

	svc := NewPermissionService(t.TempDir(), false, nil)
	dir := t.TempDir()

	for _, id := range []string{"call-1", "call-2"} {
		go func() {
			_, _ = svc.Request(t.Context(), CreatePermissionRequest{
				SessionID:  "sess-1",
				ToolCallID: id,
				ToolName:   "bash",
				Action:     "execute",
				Path:       dir,
			})
		}()
	}

	var first PermissionRequest
	require.Eventually(t, func() bool {
		var ok bool
		first, ok = svc.ActiveRequest()
		return ok
	}, 5*time.Second, 5*time.Millisecond)

	require.True(t, svc.Deny(first))

	require.Eventually(t, func() bool {
		next, ok := svc.ActiveRequest()
		return ok && next.ID != first.ID
	}, 5*time.Second, 5*time.Millisecond,
		"once the current request resolves, the next one becomes the recoverable one")
}
