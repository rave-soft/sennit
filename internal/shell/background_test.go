package shell

import (
	"context"
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

func TestBackgroundShellManagerRemoveAndKillUnknown(t *testing.T) {
	manager := NewBackgroundShellManager()
	job, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 5", "")
	require.NoError(t, err)
	require.NoError(t, manager.Remove(job.ID))
	_, found := manager.Get(job.ID)
	require.False(t, found)
	require.EqualError(t, manager.Remove(job.ID), "background shell not found: "+job.ID)
	require.EqualError(t, manager.Kill("missing"), "background shell not found: missing")
	job.cancel()
	job.Wait()
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
