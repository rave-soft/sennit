package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestSameSkillsUsesEffectiveRuntimeFields(t *testing.T) {
	base := &skills.Skill{Name: "review", Description: "Review changes", Instructions: "inspect", SkillFilePath: "/skills/review/SKILL.md", Metadata: map[string]string{"owner": "dev"}}
	equivalent := *base
	equivalent.Source = "new source transport payload"
	equivalent.Path = "/different/discovery/path"
	require.True(t, sameSkills([]*skills.Skill{base}, []*skills.Skill{&equivalent}))

	for name, change := range map[string]func(*skills.Skill){
		"instructions":   func(s *skills.Skill) { s.Instructions = "different instructions" },
		"metadata":       func(s *skills.Skill) { s.Metadata = map[string]string{"owner": "ops"} },
		"license":        func(s *skills.Skill) { s.License = "MIT" },
		"compatibility":  func(s *skills.Skill) { s.Compatibility = "linux" },
		"user_invocable": func(s *skills.Skill) { s.UserInvocable = true },
	} {
		t.Run("ignores_"+name, func(t *testing.T) {
			changed := equivalent
			change(&changed)
			require.True(t, sameSkills([]*skills.Skill{base}, []*skills.Skill{&changed}))
		})
	}
	for name, change := range map[string]func(*skills.Skill){
		"description": func(s *skills.Skill) { s.Description = "Different description" },
		"invocation":  func(s *skills.Skill) { s.DisableModelInvocation = true },
		"location":    func(s *skills.Skill) { s.SkillFilePath = "/other/SKILL.md" },
		"builtin":     func(s *skills.Skill) { s.Builtin = true },
	} {
		t.Run("detects_"+name, func(t *testing.T) {
			changed := equivalent
			change(&changed)
			require.False(t, sameSkills([]*skills.Skill{base}, []*skills.Skill{&changed}))
		})
	}
}

func TestRuntimeCacheMissReasonsAndInvalidationBoundEntries(t *testing.T) {
	cache := newRuntimeCache()
	var current atomic.Uint64
	current.Store(1)
	build := func(_ context.Context, key runtimeKey) (*compiledRuntime, error) {
		return &compiledRuntime{key: key}, nil
	}
	key := func() runtimeKey { return runtimeKey{config: current.Load()} }

	_, err := cache.getOrBuild(context.Background(), key, build)
	require.NoError(t, err)
	cache.mu.Lock()
	require.Equal(t, "config_changed", cache.missReasonLocked(runtimeKey{config: 2}))
	cache.mu.Unlock()

	current.Store(2)
	cache.invalidate(context.Background(), "config_reload", key())
	cache.mu.Lock()
	require.Equal(t, "config_reload", cache.missReasonLocked(key()))
	cache.mu.Unlock()
	_, err = cache.getOrBuild(context.Background(), key, build)
	require.NoError(t, err)
	cache.mu.Lock()
	require.Len(t, cache.entries, 1)
	require.Empty(t, cache.pendingReason)
	cache.mu.Unlock()
}

type mapThreadManager map[string]string

func (mapThreadManager) Create(context.Context, tools.ThreadCreateArgs) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}
func (mapThreadManager) List(context.Context) ([]tools.ThreadInfo, error) { return nil, nil }
func (mapThreadManager) Get(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (mapThreadManager) Cancel(context.Context, string, string) error { return nil }

func (mapThreadManager) Send(context.Context, string, string) (tools.SendOutcome, error) {
	return tools.SendOutcome{}, nil
}
func (mapThreadManager) Wait(context.Context, []string, time.Duration) error { return nil }
func (mapThreadManager) Merge(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}
func (mapThreadManager) Remove(context.Context, string, bool, bool) error { return nil }

type structThreadManager struct {
	mapThreadManager
	values []string
}

type closureThreadManager func()

func (m closureThreadManager) Create(context.Context, tools.ThreadCreateArgs) (tools.ThreadInfo, error) {
	m()
	return tools.ThreadInfo{}, nil
}
func (closureThreadManager) List(context.Context) ([]tools.ThreadInfo, error) { return nil, nil }
func (closureThreadManager) Get(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (closureThreadManager) Cancel(context.Context, string, string) error { return nil }

func (closureThreadManager) Send(context.Context, string, string) (tools.SendOutcome, error) {
	return tools.SendOutcome{}, nil
}
func (closureThreadManager) Wait(context.Context, []string, time.Duration) error { return nil }
func (closureThreadManager) Merge(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}
func (closureThreadManager) Remove(context.Context, string, bool, bool) error { return nil }

type sliceThreadManager []string

func (sliceThreadManager) Create(context.Context, tools.ThreadCreateArgs) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}
func (sliceThreadManager) List(context.Context) ([]tools.ThreadInfo, error) { return nil, nil }
func (sliceThreadManager) Get(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (sliceThreadManager) Cancel(context.Context, string, string) error { return nil }

func (sliceThreadManager) Send(context.Context, string, string) (tools.SendOutcome, error) {
	return tools.SendOutcome{}, nil
}
func (sliceThreadManager) Wait(context.Context, []string, time.Duration) error { return nil }
func (sliceThreadManager) Merge(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}
func (sliceThreadManager) Remove(context.Context, string, bool, bool) error { return nil }

func TestSetThreadsManagerIdentity(t *testing.T) {
	coordinator := &coordinator{}
	coordinator.newCoordinatorComponents()
	builder := coordinator.builder
	first := mapThreadManager{"id": "one"}
	coordinator.SetDelegationTools(first, nil)
	require.Equal(t, uint64(1), builder.localVersion.Load())
	coordinator.SetDelegationTools(first, nil)
	require.Equal(t, uint64(1), builder.localVersion.Load(), "same map identity is a no-op")
	coordinator.SetDelegationTools(mapThreadManager{"id": "one"}, nil)
	require.Equal(t, uint64(2), builder.localVersion.Load(), "different maps rebuild")

	slice := sliceThreadManager{"one"}
	coordinator.SetDelegationTools(slice, nil)
	coordinator.SetDelegationTools(slice, nil)
	require.Equal(t, uint64(4), builder.localVersion.Load(), "slices conservatively rebuild")

	unknown := structThreadManager{mapThreadManager: mapThreadManager{"id": "one"}, values: []string{"one"}}
	coordinator.SetDelegationTools(unknown, nil)
	coordinator.SetDelegationTools(unknown, nil)
	require.Equal(t, uint64(6), builder.localVersion.Load(), "unknown non-comparable structs rebuild")

	closure := closureThreadManager(func() {})
	coordinator.SetDelegationTools(closure, nil)
	coordinator.SetDelegationTools(closure, nil)
	require.Equal(t, uint64(8), builder.localVersion.Load(), "function managers conservatively rebuild even when the closure pointer matches")
}

func TestRuntimeCacheLogsLifecycleAndCorrelation(t *testing.T) {
	logs := captureLogs(t)
	cache := newRuntimeCache()
	ctx := WithRunID(context.WithValue(context.Background(), tools.SessionIDContextKey, "session-1"), "run-1")
	key := runtimeKey{config: 1}
	_, err := cache.getOrBuild(ctx, func() runtimeKey { return key }, func(context.Context, runtimeKey) (*compiledRuntime, error) { return &compiledRuntime{key: key}, nil })
	require.NoError(t, err)
	_, err = cache.getOrBuild(ctx, func() runtimeKey { return key }, nil)
	require.NoError(t, err)
	cache.invalidate(ctx, "skills_changed", runtimeKey{config: 1, local: 1})
	output := logs.String()
	for _, field := range []string{"level=DEBUG", "level=INFO", "event=miss", "event=build", "event=hit", "event=invalidate", "reason=skills_changed", "session_id=session-1", "run_id=run-1"} {
		require.Contains(t, output, field)
	}
	require.False(t, strings.Contains(output, "reason=threads_changed"))
}

func TestCoordinatorInvalidationKeepsNewestReasonAndGeneration(t *testing.T) {
	builder := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	firstMutated := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		builder.invalidateRuntime(context.Background(), "threads_changed", func() bool {
			close(firstMutated)
			<-releaseFirst
			return true
		})
		close(firstDone)
	}()
	<-firstMutated
	secondDone := make(chan struct{})
	go func() {
		builder.invalidateRuntime(context.Background(), "skills_changed", func() bool { return true })
		close(secondDone)
	}()
	close(releaseFirst)
	<-firstDone
	<-secondDone

	key := builder.runtimeKey()
	require.Equal(t, uint64(2), key.local)
	builder.runtime.mu.Lock()
	require.Equal(t, "skills_changed", builder.runtime.missReasonLocked(key))
	builder.runtime.mu.Unlock()
}

func TestCoordinatorInvalidationDoesNotWaitForRuntimeBuild(t *testing.T) {
	builder := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	buildDone := make(chan error, 1)
	go func() {
		_, err := builder.runtime.getOrBuild(context.Background(), builder.runtimeKey, func(_ context.Context, key runtimeKey) (*compiledRuntime, error) {
			startOnce.Do(func() { close(started) })
			if key.local == 0 {
				<-release
			}
			return &compiledRuntime{key: key}, nil
		})
		buildDone <- err
	}()
	<-started

	mutationDone := make(chan struct{})
	go func() {
		builder.invalidateRuntime(context.Background(), "threads_changed", func() bool { return true })
		close(mutationDone)
	}()
	select {
	case <-mutationDone:
	case <-time.After(time.Second):
		t.Fatal("runtime mutation blocked behind in-flight build")
	}
	require.Equal(t, uint64(1), builder.localVersion.Load())
	builder.runtime.mu.Lock()
	require.Equal(t, "threads_changed", builder.runtime.pendingReason[runtimeKey{local: 1}])
	builder.runtime.mu.Unlock()

	close(release)
	require.NoError(t, <-buildDone)
}

func TestRuntimeCacheWaiterCancellationDoesNotBlockMutation(t *testing.T) {
	builder := &runtimeBuilder{agentDeps: &agentDeps{}, runtime: newRuntimeCache()}
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	builderDone := make(chan error, 1)
	go func() {
		_, err := builder.runtime.getOrBuild(context.Background(), builder.runtimeKey, func(_ context.Context, key runtimeKey) (*compiledRuntime, error) {
			startOnce.Do(func() { close(started) })
			if key.local == 0 {
				<-release
			}
			return &compiledRuntime{key: key}, nil
		})
		builderDone <- err
	}()
	<-started

	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := builder.runtime.getOrBuild(waiterCtx, builder.runtimeKey, nil)
		waiterDone <- err
	}()
	cancel()
	require.ErrorIs(t, <-waiterDone, context.Canceled)

	mutationDone := make(chan struct{})
	go func() {
		builder.invalidateRuntime(context.Background(), "skills_changed", func() bool { return true })
		close(mutationDone)
	}()
	select {
	case <-mutationDone:
	case <-time.After(time.Second):
		t.Fatal("runtime mutation blocked after waiter cancellation")
	}

	close(release)
	require.NoError(t, <-builderDone)
}

func TestRuntimeCacheDiscardsOldInflightBuildAfterInvalidation(t *testing.T) {
	cache := newRuntimeCache()
	var generation atomic.Uint64
	generation.Store(1)
	started := make(chan struct{})
	release := make(chan struct{})
	// The nil error is not a shortcut: this test is about which build wins
	// after an invalidation, so both builds have to succeed.
	build := func(_ context.Context, key runtimeKey) (*compiledRuntime, error) { //nolint:unparam // signature fixed by getOrBuild
		if key.local == 1 {
			close(started)
			<-release
		}
		return &compiledRuntime{key: key}, nil
	}
	current := func() runtimeKey { return runtimeKey{local: generation.Load()} }
	firstDone := make(chan error, 1)
	go func() { _, err := cache.getOrBuild(context.Background(), current, build); firstDone <- err }()
	<-started
	generation.Store(2)
	cache.invalidate(context.Background(), "skills_changed", current())
	close(release)
	require.NoError(t, <-firstDone)

	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Len(t, cache.entries, 1)
	require.Contains(t, cache.entries, runtimeKey{local: 2})
	require.NotContains(t, cache.entries, runtimeKey{local: 1})
}
