package threadspawn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/thread"
)

func isolatedTaskRuntime(repoRoot string, spawner thread.Spawner) thread.IsolatedTaskRuntime {
	return func(ctx context.Context, args thread.TaskCreateArgs) (thread.TaskRuntime, error) {
		if err := ctx.Err(); err != nil {
			return thread.TaskRuntime{}, err
		}
		branch, path, base := args.Branch, args.WorktreePath, args.BaseBranch
		if path == "" || branch == "" || base == "" {
			return thread.TaskRuntime{}, fmt.Errorf("isolated task metadata is missing")
		}
		runtime := thread.TaskRuntime{Spawner: spawner}
		if args.Resume {
			if _, err := os.Stat(path); err != nil {
				return runtime, fmt.Errorf("resume task worktree: %w", err)
			}
		} else {
			runtime.Cleanup = func(ctx context.Context) error {
				return cleanupIsolatedTask(ctx, repoRoot, path, branch, base)
			}
			if err := git.WorktreeAdd(ctx, repoRoot, path, branch, base); err != nil {
				return runtime, fmt.Errorf("create task worktree: %w", err)
			}
		}
		handle, err := spawner.Spawn(ctx, path)
		runtime.Handle = handle
		if err != nil {
			return runtime, err
		}
		local, ok := handle.(*localHandle)
		if !ok {
			return runtime, fmt.Errorf("isolated delegation requires a local app")
		}
		if err := local.app.InitCoderAgentNonInteractive(ctx); err != nil {
			return runtime, err
		}
		var spec agent.DelegationExecution
		if err := json.Unmarshal([]byte(args.Execution), &spec); err != nil {
			return runtime, fmt.Errorf("decode delegation execution: %w", err)
		}
		spec.SessionID = args.SessionID
		runtime.Depth = spec.Depth
		if args.Resume {
			spec.Goal = args.Goal
		}
		run, err := agent.BuildDelegationRun(ctx, local.app.Coordinator(), spec)
		if err != nil {
			return runtime, err
		}
		runtime.Factory = adaptIsolatedRun(run)
		runtime.Followup = func(ctx context.Context, goal string) (thread.TaskRunFactory, error) {
			followup := spec
			followup.Goal = goal
			run, err := agent.BuildDelegationRun(ctx, local.app.Coordinator(), followup)
			if err != nil {
				return nil, err
			}
			return adaptIsolatedRun(run), nil
		}
		return runtime, nil
	}
}

func sharedTaskRuntime(owner func() agent.Coordinator, spawner thread.Spawner) thread.IsolatedTaskRuntime {
	return func(ctx context.Context, args thread.TaskCreateArgs) (thread.TaskRuntime, error) {
		handle, err := spawner.Spawn(ctx, "")
		runtime := thread.TaskRuntime{Handle: handle, Spawner: spawner}
		if err != nil {
			return runtime, err
		}
		var spec agent.DelegationExecution
		if err := json.Unmarshal([]byte(args.Execution), &spec); err != nil {
			return runtime, err
		}
		spec.SessionID = args.SessionID
		spec.Goal = args.Goal
		runtime.Depth = spec.Depth
		runtime.Followup = func(ctx context.Context, goal string) (thread.TaskRunFactory, error) {
			selected := spec
			selected.Goal = goal
			run, err := agent.BuildDelegationRun(ctx, owner(), selected)
			if err != nil {
				return nil, err
			}
			return adaptIsolatedRun(run), nil
		}
		runtime.Factory, err = runtime.Followup(ctx, args.Goal)
		return runtime, err
	}
}

func adaptIsolatedRun(run func(context.Context) (tools.TaskRunResult, error)) thread.TaskRunFactory {
	return func(context.Context, string) (func(context.Context) (thread.TaskRunResult, error), func(), error) {
		return func(ctx context.Context) (thread.TaskRunResult, error) {
			result, err := run(ctx)
			return thread.TaskRunResult{Text: result.Text}, err
		}, nil, nil
	}
}

func cleanupIsolatedTask(ctx context.Context, repoRoot, path, branch, base string) error {
	cleanupCtx := context.WithoutCancel(ctx)
	if _, err := os.Stat(path); err == nil {
		dirty, err := git.IsDirty(cleanupCtx, path)
		if err != nil {
			return fmt.Errorf("inspect task worktree before cleanup: %w", err)
		}
		if dirty {
			return fmt.Errorf("task worktree contains changes and was preserved")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect task worktree: %w", err)
	}
	exists, err := git.BranchExists(cleanupCtx, repoRoot, branch)
	if err != nil {
		return fmt.Errorf("inspect task branch: %w", err)
	}
	if exists {
		merged, err := git.IsAncestor(cleanupCtx, repoRoot, branch, base)
		if err != nil {
			return fmt.Errorf("inspect task branch ancestry: %w", err)
		}
		if !merged {
			return fmt.Errorf("task branch contains unique commits and was preserved")
		}
	}
	if err := git.WorktreeRemove(cleanupCtx, repoRoot, path, false); err != nil {
		return fmt.Errorf("remove task worktree: %w", err)
	}
	if err := git.DeleteBranch(cleanupCtx, repoRoot, branch, true); err != nil {
		return fmt.Errorf("delete task branch: %w", err)
	}
	return nil
}
