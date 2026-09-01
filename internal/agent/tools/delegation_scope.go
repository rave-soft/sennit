package tools

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

// DelegationKind is which of the two delegation shapes an id names. Both
// are the same row in internal/thread's store (a Delegation with a Kind),
// and the agent_* tools address them through one surface; the kind still
// decides what can be asked of one, since only a thread has a worktree to
// merge and only a task has a transcript this workspace can read.
type DelegationKind string

const (
	// KindTask is a background delegation sharing this workspace.
	KindTask DelegationKind = "task"
	// KindThread is a delegation running in its own git worktree.
	KindThread DelegationKind = "thread"
)

// delegationRef is one resolved delegation: which kind it turned out to
// be, and whichever of the two records carries its detail.
type delegationRef struct {
	Kind   DelegationKind
	Task   TaskInfo
	Thread ThreadInfo
}

// delegationView is the caller's view of every delegation it may act on:
// its own task subtree (see taskScope) plus the workspace's threads.
//
// The two are scoped differently on purpose. Tasks form a tree and a
// caller reaches only its own branch of it — the rule taskScope exists
// for. Threads are flat and workspace-wide: they are created by the
// top-level agent only (the thread tools are gated to it), so there is no
// tree to get lost in.
type delegationView struct {
	scope   taskScope
	threads ThreadManager
}

// resolveDelegations is the preamble every agent_* tool shares: place the
// caller in the task tree, and remember the thread manager (nil in a
// workspace with no threads). A non-nil failed is the response the tool
// must return as-is.
func resolveDelegations(ctx context.Context, tasks TaskManager, threads ThreadManager, action string) (view delegationView, failed *fantasy.ToolResponse, err error) {
	scope, failed, err := scopeTasks(ctx, tasks, action)
	if err != nil || failed != nil {
		return delegationView{}, failed, err
	}
	return delegationView{scope: scope, threads: threads}, nil, nil
}

// lookup resolves idOrName to a delegation the caller may act on. A task
// id is checked against the caller's subtree first, so a caller can never
// reach a task that is not its own; only then is idOrName offered to the
// thread manager, which also accepts a thread's name.
//
// A non-nil refusal is the tool's answer: either the id is not the
// caller's to touch (taskScope.refuse says why, in the terms the model
// needs) or nothing by that name exists at all.
func (v delegationView) lookup(ctx context.Context, tasks TaskManager, idOrName, verb string) (delegationRef, *fantasy.ToolResponse) {
	if _, mine := v.scope.descendants[idOrName]; mine {
		ti, err := tasks.Get(ctx, idOrName)
		if err != nil {
			resp := fantasy.NewTextErrorResponse(err.Error())
			return delegationRef{}, &resp
		}
		return delegationRef{Kind: KindTask, Task: ti}, nil
	}
	if v.threads != nil {
		st, err := v.threads.Get(ctx, idOrName)
		switch {
		case err == nil:
			return delegationRef{Kind: KindThread, Thread: st}, nil
		case !errors.Is(err, ErrThreadNotFound):
			resp := fantasy.NewTextErrorResponse(err.Error())
			return delegationRef{}, &resp
		}
	}
	// Not a thread either. The caller's own id and its parent's are their
	// own mistakes with their own answers; anything else is simply not
	// here, and what that most likely means depends on whether there were
	// threads to look through at all.
	if refusal, refused := v.scope.refuseOwnOrAncestor(idOrName, verb); refused {
		return delegationRef{}, &refusal
	}
	refusal := v.scope.refuseUnknown(idOrName, verb, v.threads != nil)
	return delegationRef{}, &refusal
}

// unsupported is the answer when a delegation exists but its kind cannot
// do what was asked. It names the kind and what to use instead, because
// the caller reached the right delegation by the right id and needs the
// next step, not a bare refusal.
func unsupported(ref delegationRef, what, instead string) fantasy.ToolResponse {
	return fantasy.NewTextErrorResponse(fmt.Sprintf(
		"%s is a %s, and %s. %s", delegationID(ref), ref.Kind, what, instead))
}

// delegationID is how a resolved delegation is addressed in prose.
func delegationID(ref delegationRef) string {
	if ref.Kind == KindThread {
		return ref.Thread.ID
	}
	return ref.Task.ID
}
