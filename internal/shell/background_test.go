package shell

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func insertBackgroundShellForTest(m *BackgroundShellManager, id string, done bool) *BackgroundShell {
	job := &BackgroundShell{ID: id, done: make(chan struct{}), cancel: func() {}}
	if done {
		close(job.done)
	}
	m.mu.Lock()
	m.shells[id] = job
	m.mu.Unlock()
	return job
}

func TestBackgroundShellManagerStart(t *testing.T) {
	manager := NewBackgroundShellManager()
	job, err := manager.Start(t.Context(), t.TempDir(), nil, "echo hello", "")
	require.NoError(t, err)
	job.Wait()
	stdout, stderr, done, err := job.GetOutput()
	require.True(t, done)
	require.NoError(t, err)
	require.Contains(t, stdout, "hello")
	require.Empty(t, stderr)
}

func TestBackgroundShellManagerStartVsShutdown(t *testing.T) {
	manager := NewBackgroundShellManager()
	workingDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.startHook = func() {
		close(entered)
		<-release
	}
	started := make(chan struct {
		job *BackgroundShell
		err error
	}, 1)
	go func() {
		job, err := manager.Start(context.Background(), workingDir, nil, "sleep 5", "")
		started <- struct {
			job *BackgroundShell
			err error
		}{job, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("start did not enter admission")
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- manager.Shutdown(context.Background()) }()
	close(release)
	result := <-started
	require.NoError(t, result.err)
	require.NotNil(t, result.job)
	require.NoError(t, <-shutdown)
	require.True(t, result.job.IsDone())
	_, err := manager.Start(t.Context(), workingDir, nil, "echo late", "")
	require.EqualError(t, err, "background shell manager is shut down")
}

func TestBackgroundShellManagerConcurrentShutdownDeadlines(t *testing.T) {
	manager := NewBackgroundShellManager()
	job := insertBackgroundShellForTest(manager, "stuck", false)
	short, cancelShort := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelShort()
	long, cancelLong := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancelLong()
	errs := make(chan error, 2)
	go func() { errs <- manager.Shutdown(short) }()
	go func() { errs <- manager.Shutdown(long) }()
	require.ErrorIs(t, <-errs, context.DeadlineExceeded)
	require.ErrorIs(t, <-errs, context.DeadlineExceeded)
	close(job.done)
	require.NoError(t, manager.Shutdown(t.Context()))
	require.Equal(t, BackgroundJobCounts{}, manager.Counts())
	_, err := manager.Start(t.Context(), t.TempDir(), nil, "echo late", "")
	require.EqualError(t, err, "background shell manager is shut down")
}

func TestBackgroundShellManagerRemoveUnknown(t *testing.T) {
	manager := NewBackgroundShellManager()
	require.EqualError(t, manager.Remove("missing"), "background shell not found: missing")
	require.EqualError(t, manager.Kill("missing"), "background shell not found: missing")
}

func TestBackgroundShellManagerRemoveSucceedsForFinishedJob(t *testing.T) {
	manager := NewBackgroundShellManager()
	job := insertBackgroundShellForTest(manager, "finished", true)
	require.NoError(t, manager.Remove(job.ID))
	_, found := manager.Get(job.ID)
	require.False(t, found)
	require.EqualError(t, manager.Remove(job.ID), "background shell not found: "+job.ID)
}

// TestBackgroundShellManagerRemoveNotFoundIsARaceWithAdmissionSweep covers
// the sequence that makes not-found reachable on a success path: a job
// finishes, Start's admission sweep (removeCompletedLocked, triggered by the
// table being at MaxBackgroundJobs) reaps it before the job's "owner" calls
// Remove itself, and that later Remove must come back as
// ErrBackgroundShellNotFound rather than some caller-visible failure.
func TestBackgroundShellManagerRemoveNotFoundIsARaceWithAdmissionSweep(t *testing.T) {
	manager := NewBackgroundShellManager()
	job := insertBackgroundShellForTest(manager, "already-swept", true)

	// Stand in for removeCompletedLocked already having reaped this job.
	manager.mu.Lock()
	delete(manager.shells, job.ID)
	manager.mu.Unlock()

	err := manager.Remove(job.ID)
	require.ErrorIs(t, err, ErrBackgroundShellNotFound)
	require.False(t, errors.Is(err, ErrBackgroundShellStillRunning))
}

// TestBackgroundShellManagerRemoveRefusesRunningJob is the regression test
// for the Remove defect: dropping a running job's map entry would make it
// unreachable from Kill/Cleanup/Shutdown, orphaning its process for the rest
// of the process lifetime. Remove must refuse, and — the actual point of the
// fix — the job must still be there afterward for Shutdown to find and stop.
func TestBackgroundShellManagerRemoveRefusesRunningJob(t *testing.T) {
	manager := NewBackgroundShellManager()
	job := &BackgroundShell{ID: "running", done: make(chan struct{})}
	job.cancel = func() { close(job.done) } // stands in for the real process exiting once canceled
	manager.mu.Lock()
	manager.shells[job.ID] = job
	manager.mu.Unlock()

	require.EqualError(t, manager.Remove(job.ID), "background shell still running: "+job.ID)
	_, found := manager.Get(job.ID)
	require.True(t, found, "job must still be reachable after a refused Remove")

	// The point of the fix: Shutdown must still find this job and stop it.
	require.NoError(t, manager.Shutdown(t.Context()))
	require.True(t, job.IsDone())
}

func TestBackgroundShellManagerCleanupRetention(t *testing.T) {
	manager := NewBackgroundShellManager()
	old := insertBackgroundShellForTest(manager, "old", true)
	old.completedAt.Store(time.Now().Add(-(CompletedJobRetentionMinutes + 1) * time.Minute).Unix())
	recent := insertBackgroundShellForTest(manager, "recent", true)
	recent.completedAt.Store(time.Now().Unix())
	require.Equal(t, 1, manager.Cleanup())
	_, found := manager.Get("old")
	require.False(t, found)
	_, found = manager.Get("recent")
	require.True(t, found)
}

func TestBackgroundShellManagerLimitReclaimsCompletedAndRejectsRunning(t *testing.T) {
	workingDir := t.TempDir()
	completed := NewBackgroundShellManager()
	for i := range MaxBackgroundJobs {
		insertBackgroundShellForTest(completed, fmt.Sprintf("done-%d", i), true)
	}
	job, err := completed.Start(t.Context(), workingDir, nil, "echo reclaimed", "")
	require.NoError(t, err)
	job.Wait()
	require.Len(t, completed.List(), 1)

	running := NewBackgroundShellManager()
	for i := range MaxBackgroundJobs {
		insertBackgroundShellForTest(running, fmt.Sprintf("running-%d", i), false)
	}
	_, err = running.Start(t.Context(), workingDir, nil, "echo blocked", "")
	require.EqualError(t, err, "maximum number of background jobs (50) reached. Please terminate or wait for some jobs to complete")
}

func TestBackgroundShellManagerGetListAndCounts(t *testing.T) {
	manager := NewBackgroundShellManager()
	active := insertBackgroundShellForTest(manager, "B", false)
	completed := insertBackgroundShellForTest(manager, "A", true)
	got, found := manager.Get(active.ID)
	require.True(t, found)
	require.Same(t, active, got)
	require.Equal(t, []string{"A", "B"}, manager.List())
	require.Equal(t, BackgroundJobCounts{Active: 1, Completed: 1}, manager.Counts())
	require.Same(t, completed, func() *BackgroundShell { job, _ := manager.Get("A"); return job }())
}

func TestBackgroundShellWaitContext(t *testing.T) {
	completed := &BackgroundShell{done: make(chan struct{})}
	close(completed.done)
	require.True(t, completed.WaitContext(t.Context()))
	pending := &BackgroundShell{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.False(t, pending.WaitContext(ctx))
}

func TestBackgroundShellManagerConcurrentAdmission(t *testing.T) {
	manager := NewBackgroundShellManager()
	workingDir := t.TempDir()
	var wg sync.WaitGroup
	jobs := make(chan *BackgroundShell, MaxBackgroundJobs+20)
	for range MaxBackgroundJobs + 20 {
		wg.Go(func() {
			job, err := manager.Start(context.Background(), workingDir, nil, "sleep 5", "")
			if err == nil {
				jobs <- job
			}
		})
	}
	wg.Wait()
	close(jobs)
	require.Len(t, jobs, MaxBackgroundJobs)
	require.NoError(t, manager.Shutdown(t.Context()))
}
