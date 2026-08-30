package model

import (
	"context"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
)

// threadsTestWorkspace is internal/ui/threads' fake of the same name.
// Test files are not importable, so the copy is the only way to share a
// fixture across the two packages; each is free to grow only what its
// own tests need.

// threadsTestWorkspace is a minimal workspace.Workspace stub for exercising
// the thread list cache, following the testWorkspace pattern above (embed
// the full interface, override only what's exercised).
type threadsTestWorkspace struct {
	workspace.Workspace
	threads   []proto.Thread
	err       error
	supported bool
	calls     int

	// AttachThread-specific: kept separate from err/threads so tests that
	// exercise ListThreads don't need to care about them.
	attachWS    workspace.Workspace
	attachErr   error
	detachCalls int

	// Task-specific, kept separate from threads/err so tests that only
	// exercise ListThreads don't need to care about them.
	taskSupported bool
	tasks         []proto.Thread
	taskErr       error
	taskCalls     int

	// cancelTaskCalls records every id CancelTask was invoked with, so
	// tests can assert both "called exactly once" and "called with the
	// right id, not some other delegation's".
	cancelTaskCalls []string
	cancelTaskErr   error

	// cancelThreadCalls is CancelTask's sibling for CancelThread, kept
	// separate so a test can assert the router picked the *right* one of
	// the two for a given delegation's kind.
	cancelThreadCalls []string
	cancelThreadErr   error
}

func (w *threadsTestWorkspace) SupportsThreads() bool { return w.supported }

func (w *threadsTestWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	w.calls++
	return w.threads, w.err
}

func (w *threadsTestWorkspace) SupportsTasks() bool { return w.taskSupported }

func (w *threadsTestWorkspace) ListTasks(context.Context) ([]proto.Thread, error) {
	w.taskCalls++
	return w.tasks, w.taskErr
}

func (w *threadsTestWorkspace) CancelTask(_ context.Context, id, _ string) error {
	w.cancelTaskCalls = append(w.cancelTaskCalls, id)
	return w.cancelTaskErr
}

func (w *threadsTestWorkspace) CancelThread(_ context.Context, id, _ string) error {
	w.cancelThreadCalls = append(w.cancelThreadCalls, id)
	return w.cancelThreadErr
}

// The following ThreadController methods round out threadsTestWorkspace for
// root_test.go, which drives the router through attach/merge/remove/create
// rather than just ListThreads.

func (w *threadsTestWorkspace) AttachThread(context.Context, string) (workspace.Workspace, func(), error) {
	return w.attachWS, func() { w.detachCalls++ }, w.attachErr
}

func (w *threadsTestWorkspace) CreateThread(context.Context, proto.CreateThreadRequest) (proto.Thread, error) {
	return proto.Thread{}, w.err
}

func (w *threadsTestWorkspace) MergeThread(context.Context, string) (proto.Thread, error) {
	return proto.Thread{}, w.err
}

func (w *threadsTestWorkspace) RemoveThread(context.Context, string, proto.RemoveThreadOptions) error {
	return w.err
}
