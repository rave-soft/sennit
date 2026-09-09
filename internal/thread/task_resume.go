package thread

import (
	"context"
	"errors"
	"fmt"
)

func (t *TaskManager) sendIsolated(ctx context.Context, st Thread, message string) (disposition SendDisposition, resultErr error) {
	control := t.lc.control(st.ID)
	control.opMu.Lock()
	defer control.opMu.Unlock()
	current, err := t.store.Get(ctx, st.ID)
	if err != nil {
		return disposition, err
	}
	if wasCancelled(current) {
		return disposition, fmt.Errorf("thread: task %q was cancelled and cannot be resumed", st.ID)
	}
	control.mu.Lock()
	runtime := control.runtime
	removed := control.removed
	control.mu.Unlock()
	if removed {
		return disposition, fmt.Errorf("thread: task %q was removed", st.ID)
	}
	if runtime != nil && runtime.releaseFailed {
		if err := releaseRuntime(ctx, runtime, st.SessionID, true); err != nil {
			return disposition, fmt.Errorf("thread: release previous isolated runtime: %w", err)
		}
		control.mu.Lock()
		control.runtime = nil
		control.mu.Unlock()
		runtime = nil
	}
	if runtime != nil {
		if runtime.followup == nil {
			return disposition, errors.New("thread: isolated follow-up builder is unavailable")
		}
		factory, err := runtime.followup(ctx, message)
		if err != nil {
			return disposition, err
		}
		control.mu.Lock()
		ahead := len(runtime.queuedRuns)
		runtime.queuedRuns = append(runtime.queuedRuns, factory)
		control.mu.Unlock()
		return SendDisposition{Queued: true, Ahead: ahead}, nil
	}
	buildRuntime := t.isolated
	isolation := "worktree"
	if st.WorktreePath == "" {
		buildRuntime = t.shared
		isolation = ""
	}
	if buildRuntime == nil || st.Execution == "" {
		return disposition, errors.New("thread: isolated task runtime is unavailable; its worktree has been preserved")
	}
	t.createMu.Lock()
	if err := t.checkActiveCaps(ctx, st.ParentSessionID, message); err != nil {
		t.createMu.Unlock()
		return disposition, err
	}
	pending, err := t.lc.setStatus(ctx, st.ID, StatusPending, "", "", 0)
	t.createMu.Unlock()
	if err != nil {
		return disposition, err
	}
	prepCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.ctx, cancel)
	defer stop()
	defer cancel()
	control.mu.Lock()
	control.preparationCancel = cancel
	if control.cancelRequested {
		cancel()
	}
	control.mu.Unlock()
	defer func() {
		control.mu.Lock()
		control.preparationCancel = nil
		control.mu.Unlock()
	}()
	prepared, err := buildRuntime(prepCtx, TaskCreateArgs{
		Goal: message, ParentSessionID: st.ParentSessionID, SessionID: st.SessionID,
		DelegationID: st.ID, Isolation: isolation, Execution: st.Execution, Resume: true,
		WorktreePath: st.WorktreePath, Branch: st.Branch, BaseBranch: st.BaseBranch,
	})
	transferred := false
	defer func() {
		if !transferred && prepared.Handle != nil && prepared.Spawner != nil {
			cleanupCtx, release := detachForTerminalWork(ctx)
			defer release()
			if err := prepared.Spawner.Release(cleanupCtx, prepared.Handle.ID()); err != nil {
				control.mu.Lock()
				control.runtime = &runtimeState{handle: prepared.Handle, spawner: prepared.Spawner, watchCancel: func() {}, releaseFailed: true}
				control.mu.Unlock()
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	if err != nil {
		return disposition, t.failCreate(prepCtx, pending, err)
	}
	if prepared.Handle == nil || prepared.Spawner == nil || prepared.Factory == nil {
		return disposition, t.failCreate(prepCtx, pending, errors.New("thread: isolated resume runtime is incomplete"))
	}
	control.mu.Lock()
	preparationErr := prepCtx.Err()
	control.preparationCancel = nil
	control.depth = prepared.Depth
	control.parentSessionID = st.ParentSessionID
	control.mu.Unlock()
	if preparationErr != nil {
		return disposition, t.failCreate(prepCtx, pending, preparationErr)
	}
	running, err := t.lc.setStatus(prepCtx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		return disposition, t.failCreate(prepCtx, pending, err)
	}
	coordinator := prepared.Handle.Workspace().Coordinator()
	registerParent(coordinator, coordinator, running, prepared.Depth)
	runCtx := t.lc.withDelegation(t.ctx, st.ID)
	t.lc.startFactoryRun(runCtx, prepared.Handle, prepared.Spawner, st.ID, st.SessionID, prepared.Factory)
	control.mu.Lock()
	control.runtime.followup = prepared.Followup
	control.mu.Unlock()
	transferred = true
	return SendDisposition{Resumed: true}, nil
}
