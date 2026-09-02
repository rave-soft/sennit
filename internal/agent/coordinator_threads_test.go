package agent

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// noopThreadManager is a minimal tools.ThreadManager with no behavior:
// these tests only check whether the thread_* tools are offered to an
// agent, not what they do (see internal/agent/tools/thread_tools_test.go
// and internal/thread for that).
type noopThreadManager struct{}

func (noopThreadManager) Create(context.Context, tools.ThreadCreateArgs) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (noopThreadManager) List(context.Context) ([]tools.ThreadInfo, error) { return nil, nil }

func (noopThreadManager) Get(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (noopThreadManager) Cancel(context.Context, string, string) error { return nil }

func (noopThreadManager) Send(context.Context, string, string) (tools.SendOutcome, error) {
	return tools.SendOutcome{}, nil
}

func (noopThreadManager) Wait(context.Context, []string, time.Duration) error {
	return nil
}

func (noopThreadManager) Merge(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (noopThreadManager) Remove(context.Context, string, bool, bool) error {
	return nil
}

// threadToolNames lists the tools a workspace with a thread manager
// expects under the coder agent's *default* AllowedTools: the worktree
// lifecycle, plus the agent_* tools that answer for threads as well as
// background tasks.
var threadToolNames = []string{
	tools.ThreadCreateToolName,
	tools.AgentListToolName,
	tools.AgentResultToolName,
	tools.AgentSendToolName,
	tools.ThreadMergeToolName,
	tools.ThreadRemoveToolName,
}

// newThreadsTestCoordinator builds a coordinator with the minimal
// dependencies buildTools touches, wired against the given thread
// manager (nil-able).
func newThreadsTestCoordinator(t *testing.T, threads tools.ThreadManager) (*coordinator, config.Agent) {
	t.Helper()
	env := testEnv(t)

	// Minimal hermetic config: one openai-typed provider with a selected
	// model, so buildAgentModel (reached through the
	// "agent" delegation tool that buildTools always tries to build)
	// succeeds without any real network access.
	sennitJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	writeGlobalConfig(t, sennitJSON)

	cfg, err := configruntime.Load(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		mcp:         mcp.NewRegistry(),
		background:  shell.NewBackgroundShellManager(),
		questions:   question.NewService(),
	}
	coord.newCoordinatorComponents()
	coord.SetDelegationTools(threads, nil)
	return coord, cfg.Config().Agents[config.AgentCoder]
}

func toolNames(t *testing.T, agentTools []fantasy.AgentTool) []string {
	t.Helper()
	names := make([]string, len(agentTools))
	for i, tool := range agentTools {
		names[i] = tool.Info().Name
	}
	return names
}

func TestBuildTools_ThreadToolsPresentForMainAgentWithManager(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, noopThreadManager{})

	built, err := coord.builder.buildTools(t.Context(), agentCfg, false, coord.delegation.runtimeInputs())
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, want := range threadToolNames {
		require.Contains(t, names, want)
	}
}

func TestBuildTools_ThreadToolsAbsentWhenManagerNil(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, nil)

	built, err := coord.builder.buildTools(t.Context(), agentCfg, false, coord.delegation.runtimeInputs())
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range threadToolNames {
		require.NotContains(t, names, absent)
	}
}

func TestBuildTools_ThreadToolsAbsentForSubAgent(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, noopThreadManager{})

	// isSubAgent=true mirrors how the coordinator builds the "agent"
	// delegation tool's target and other sub-agents: thread tools must
	// never be handed to them even when the workspace owns a manager.
	built, err := coord.builder.buildTools(t.Context(), agentCfg, true, coord.delegation.runtimeInputs())
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range threadToolNames {
		require.NotContains(t, names, absent)
	}
}

// TestBuildTools_AgentSendReachesThreadsByDefault pins what merging the
// two send tools decided. thread_send was deliberately outside the
// default set — a running thread does not read a follow-up until its
// current turn ends, so the steering it looks like it offers is not real
// — while task_send, which has exactly the same property, was inside it.
// One tool cannot be both, and the tool that exists now reports its own
// disposition (delivered now, or queued behind the turn in flight), which
// is the answer to the objection that kept thread_send out.
func TestBuildTools_AgentSendReachesThreadsByDefault(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, noopThreadManager{})

	built, err := coord.builder.buildTools(t.Context(), agentCfg, false, coord.delegation.runtimeInputs())
	require.NoError(t, err)
	require.Contains(t, toolNames(t, built), tools.AgentSendToolName,
		"one send tool for both kinds, and it is in the default set")
}

// TestBuildTools_DelegationToolsSurviveBackgroundAgentsOff covers the
// gate the merge needed: the agent_* tools answer for threads too, so
// turning background tasks off must not take a workspace's threads away
// with them.
func TestBuildTools_DelegationToolsSurviveBackgroundAgentsOff(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, noopThreadManager{})
	disabled := false
	coord.cfg.Config().Options.BackgroundAgents = &disabled

	built, err := coord.builder.buildTools(t.Context(), agentCfg, false, coord.delegation.runtimeInputs())
	require.NoError(t, err)
	names := toolNames(t, built)
	for _, name := range threadToolNames {
		require.Contains(t, names, name,
			"a workspace with threads keeps its delegation tools when background tasks are off")
	}
}

func TestCoordinator_SetDelegationToolsThreadTakesEffectOnNextBuild(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, nil)

	built, err := coord.builder.buildTools(t.Context(), agentCfg, false, coord.delegation.runtimeInputs())
	require.NoError(t, err)
	require.NotContains(t, toolNames(t, built), tools.ThreadCreateToolName)

	coord.SetDelegationTools(noopThreadManager{}, nil)

	built, err = coord.builder.buildTools(t.Context(), agentCfg, false, coord.delegation.runtimeInputs())
	require.NoError(t, err)
	require.Contains(t, toolNames(t, built), tools.ThreadCreateToolName)
}

// TestCoordinator_SetDelegationToolsPublishesOneAdapterGeneration verifies
// readers never observe a thread adapter from a different generation than the
// task adapter, even while concurrent publishers replace the pair.
func TestCoordinator_SetDelegationToolsPublishesOneAdapterGeneration(t *testing.T) {
	coord := &coordinator{builder: &runtimeBuilder{runtime: newRuntimeCache()}}
	coord.delegation = &delegationFinalizer{builder: coord.builder}
	const (
		generations = 64
		readers     = 4
		writers     = 2
	)
	threads := make([]tools.ThreadManager, generations)
	tasks := make([]tools.TaskManager, generations)
	pairs := make(map[tools.ThreadManager]tools.TaskManager, generations)
	for i := range generations {
		threads[i] = &fakeSnapshotThreadManager{id: i}
		tasks[i] = &fakeSnapshotTaskManager{id: i}
		pairs[threads[i]] = tasks[i]
	}

	// Start every worker together. Publishers keep their publication phase
	// active until a reader has checked a snapshot, guaranteeing that this
	// test exercises reader/publisher overlap rather than merely a sequence
	// of completed writes followed by a read.
	start := make(chan struct{})
	publishersDone := make(chan struct{})
	var ready, publisherWG, readerWG sync.WaitGroup
	ready.Add(readers + writers)
	publisherWG.Add(writers)
	readerWG.Add(readers)

	var activePublishers atomic.Int64
	var checkedReads atomic.Int64
	var mismatchReported atomic.Bool
	mismatches := make(chan struct{}, 1)

	for writer := range writers {
		go func() {
			defer publisherWG.Done()
			ready.Done()
			<-start
			for i := writer; i < generations; i += writers {
				readsBefore := checkedReads.Load()
				activePublishers.Add(1)
				coord.SetDelegationTools(threads[i], tasks[i])
				for checkedReads.Load() == readsBefore {
					runtime.Gosched()
				}
				activePublishers.Add(-1)
			}
		}()
	}
	go func() {
		publisherWG.Wait()
		close(publishersDone)
	}()

	for range readers {
		go func() {
			defer readerWG.Done()
			ready.Done()
			<-start
			for {
				if activePublishers.Load() == 0 {
					select {
					case <-publishersDone:
						return
					default:
						runtime.Gosched()
						continue
					}
				}

				snapshot := coord.delegation.delegationToolsForRead()
				if snapshot.threads != nil && pairs[snapshot.threads] != snapshot.tasks && mismatchReported.CompareAndSwap(false, true) {
					mismatches <- struct{}{}
				}
				checkedReads.Add(1)
			}
		}()
	}

	ready.Wait()
	close(start)
	readerWG.Wait()

	require.Positive(t, checkedReads.Load(), "concurrent readers must check at least one published generation")
	select {
	case <-mismatches:
		t.Fatal("reader observed thread and task adapters from different generations")
	default:
	}
}

type fakeSnapshotThreadManager struct{ id int }

func (*fakeSnapshotThreadManager) Create(context.Context, tools.ThreadCreateArgs) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (*fakeSnapshotThreadManager) List(context.Context) ([]tools.ThreadInfo, error) { return nil, nil }

func (*fakeSnapshotThreadManager) Get(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (*fakeSnapshotThreadManager) Cancel(context.Context, string, string) error { return nil }

func (*fakeSnapshotThreadManager) Send(context.Context, string, string) (tools.SendOutcome, error) {
	return tools.SendOutcome{}, nil
}

func (*fakeSnapshotThreadManager) Wait(context.Context, []string, time.Duration) error { return nil }

func (*fakeSnapshotThreadManager) Merge(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}
func (*fakeSnapshotThreadManager) Remove(context.Context, string, bool, bool) error { return nil }

type fakeSnapshotTaskManager struct{ id int }

func (*fakeSnapshotTaskManager) Create(context.Context, tools.TaskCreateArgs) (tools.TaskInfo, error) {
	return tools.TaskInfo{}, nil
}
func (*fakeSnapshotTaskManager) List(context.Context) ([]tools.TaskInfo, error) { return nil, nil }
func (*fakeSnapshotTaskManager) Get(context.Context, string) (tools.TaskInfo, error) {
	return tools.TaskInfo{}, nil
}
func (*fakeSnapshotTaskManager) Cancel(context.Context, string, string) error { return nil }
func (*fakeSnapshotTaskManager) Send(context.Context, string, string) (tools.SendOutcome, error) {
	return tools.SendOutcome{}, nil
}

func (*fakeSnapshotTaskManager) Output(context.Context, string, int) (tools.TaskOutput, error) {
	return tools.TaskOutput{}, nil
}
