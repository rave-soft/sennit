package model

import (
	"testing"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/stretchr/testify/require"
)

// activeThreadFixture is a running delegation of the given kind, in the
// shape activeDockThreads yields to the panel plan.
func activeThreadFixture(id, kind string) proto.Thread {
	return proto.Thread{
		ID:     id,
		Name:   id,
		Kind:   kind,
		Status: string(thread.StatusRunning),
	}
}

func incompleteTodos() []session.Todo {
	return []session.Todo{{Content: "still open", Status: session.TodoStatusPending}}
}

// TestHasRunningThread_DistinguishesKindsAndIdle pins the rule the panel depends
// on: a task is not a thread, so it must not suppress the todos section.
func TestHasRunningThread_DistinguishesKindsAndIdle(t *testing.T) {
	t.Parallel()

	require.False(t, hasRunningThread(nil))
	require.False(t, hasRunningThread([]proto.Thread{
		activeThreadFixture("t1", string(thread.KindTask)),
	}), "a background task must not count as a thread")
	require.True(t, hasRunningThread([]proto.Thread{
		activeThreadFixture("th1", string(thread.KindThread)),
	}))
	require.True(t, hasRunningThread([]proto.Thread{
		activeThreadFixture("t1", string(thread.KindTask)),
		activeThreadFixture("th1", string(thread.KindThread)),
	}), "one running thread is enough")
	require.True(t, hasRunningThread([]proto.Thread{
		activeThreadFixture("legacy", ""),
	}), "a row predating the kind column is a thread")
	require.False(t, hasRunningThread([]proto.Thread{
		{ID: "th-idle", Kind: string(thread.KindThread), Status: string(thread.StatusIdle)},
	}), "an idle thread is a parked workspace, not work in flight")
}

// TestSessionPanelPlan_RunningThreadHidesTodos covers the panel rule that
// a thread takes the todos section's place. A running thread means the
// work moved out of the main agent, so its todo list stops describing
// what is happening; a background task runs alongside that work, so it
// must leave the list alone. The todos return once the thread is no
// longer active — nothing is dropped permanently.
func TestSessionPanelPlan_RunningThreadHidesTodos(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.session.Todos = incompleteTodos()
	require.True(t, u.sessionPanelPlan(100).todosVisible,
		"with no delegations the todos section is the panel's whole point")

	u.threadsDock.cache.value = []proto.Thread{activeThreadFixture("t1", string(thread.KindTask))}
	require.True(t, u.sessionPanelPlan(100).todosVisible,
		"a background task runs alongside the main agent, so its todos stay")

	u.threadsDock.cache.value = []proto.Thread{activeThreadFixture("th1", string(thread.KindThread))}
	plan := u.sessionPanelPlan(100)
	require.False(t, plan.todosVisible, "a running thread replaces the todos section")
	require.Zero(t, plan.todosContentRows, "a hidden section must not reserve rows")
	require.NotZero(t, plan.threadsRows, "the threads section is what remains")

	u.threadsDock.cache.value = []proto.Thread{{ID: "th1", Kind: string(thread.KindThread), Status: string(thread.StatusMerged)}}
	require.True(t, u.sessionPanelPlan(100).todosVisible,
		"once no thread is running the todos come back")

	// The case that made this rule wrong on its first outing: opening a
	// finished thread reactivates it, so it sits at idle with nothing
	// running. That must not hide the main agent's todos for the rest of
	// the session.
	u.threadsDock.cache.value = []proto.Thread{{ID: "th1", Kind: string(thread.KindThread), Status: string(thread.StatusIdle)}}
	plan = u.sessionPanelPlan(100)
	require.True(t, plan.todosVisible, "an idle thread is parked, not working")
	require.NotZero(t, plan.threadsRows, "it still occupies the threads section")
}
