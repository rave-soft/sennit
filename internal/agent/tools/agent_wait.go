package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
	"golang.org/x/sync/errgroup"
)

const AgentWaitToolName = "agent_wait"

//go:embed agent_wait.md.tpl
var agentWaitDescriptionTmpl []byte

var agentWaitDescriptionTpl = template.Must(
	template.New("agentWaitDescription").Parse(string(agentWaitDescriptionTmpl)),
)

type AgentWaitParams struct {
	IDs []string `json:"ids" description:"Delegation IDs or thread names to wait for"`
}

type TaskWaiter interface {
	Wait(context.Context, []string) error
}

func NewAgentWaitTool(tasks TaskManager, threads ThreadManager) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		AgentWaitToolName,
		renderToolDescription(agentWaitDescriptionTpl),
		func(ctx context.Context, params AgentWaitParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if len(params.IDs) == 0 {
				return fantasy.NewTextErrorResponse("ids must contain at least one delegation"), nil
			}
			view, failed, err := resolveDelegations(ctx, tasks, threads, AgentWaitToolName)
			if err != nil || failed != nil {
				return derefResponse(failed), err
			}

			taskIDs := make([]string, 0, len(params.IDs))
			threadIDs := make([]string, 0, len(params.IDs))
			seen := make(map[string]struct{}, len(params.IDs))
			for _, id := range params.IDs {
				if id == "" {
					return fantasy.NewTextErrorResponse("ids must not contain an empty delegation ID"), nil
				}
				ref, refusal := view.lookup(ctx, tasks, id, "wait for")
				if refusal != nil {
					return *refusal, nil
				}
				key := string(ref.Kind) + ":" + delegationID(ref)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				if ref.Kind == KindThread {
					threadIDs = append(threadIDs, delegationID(ref))
				} else {
					taskIDs = append(taskIDs, delegationID(ref))
				}
			}

			var taskWaiter TaskWaiter
			if len(taskIDs) > 0 {
				var ok bool
				taskWaiter, ok = tasks.(TaskWaiter)
				if !ok {
					return fantasy.NewTextErrorResponse("Waiting for tasks is unavailable in this workspace."), nil
				}
			}
			input := WaitForUserInput(ctx)
			waitCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			waitGroup, waitCtx := errgroup.WithContext(waitCtx)
			if len(taskIDs) > 0 {
				waitGroup.Go(func() error {
					if err := taskWaiter.Wait(waitCtx, taskIDs); err != nil {
						return fmt.Errorf("wait for tasks: %w", err)
					}
					return nil
				})
			}
			if len(threadIDs) > 0 {
				waitGroup.Go(func() error {
					if err := threads.Wait(waitCtx, threadIDs, 0); err != nil {
						return fmt.Errorf("wait for threads: %w", err)
					}
					return nil
				})
			}
			done := make(chan error, 1)
			go func() { done <- waitGroup.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
			case <-input:
				cancel()
				<-done
				return fantasy.NewTextResponse("Wait interrupted because the person sent a message; respond to them now. Delegations continue in the background and will report automatically."), nil
			case <-ctx.Done():
				cancel()
				<-done
				return fantasy.ToolResponse{}, ctx.Err()
			}

			statuses := make([]string, 0, len(taskIDs)+len(threadIDs))
			for _, id := range taskIDs {
				ti, err := tasks.Get(ctx, id)
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("read waited task %q: %w", id, err)
				}
				statuses = append(statuses, fmt.Sprintf("Task %s status: %s", ti.ID, ti.Status))
			}
			for _, id := range threadIDs {
				st, err := threads.Get(ctx, id)
				if errors.Is(err, ErrThreadNotFound) {
					statuses = append(statuses, fmt.Sprintf("Thread %s finished and is no longer available.", id))
					continue
				}
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("read waited thread %q: %w", id, err)
				}
				statuses = append(statuses, fmt.Sprintf("Thread %s status: %s", st.ID, st.Status))
			}
			return fantasy.NewTextResponse(strings.Join(statuses, "\n")), nil
		},
	), map[string]toolParameterSchema{"ids": {minItems: intPtr(1)}, "ids.items": {minLength: intPtr(1)}})
}
