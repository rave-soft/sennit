package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
)

const ThreadWaitToolName = "thread_wait"

//go:embed thread_wait.md.tpl
var threadWaitDescriptionTmpl []byte

var threadWaitDescriptionTpl = template.Must(
	template.New("threadWaitDescription").Parse(string(threadWaitDescriptionTmpl)),
)

// activeThreadStatuses are the states thread_wait waits out. They mirror
// thread.Status.Active, which this package cannot import.
var activeThreadStatuses = []string{"pending", "running", "merging"}

type ThreadWaitParams struct {
	IDs            []string `json:"ids,omitempty" description:"Thread IDs or names to wait for (default: all)"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" description:"Give up after this many seconds (default: no timeout)"`
}

// NewThreadWaitTool creates the thread_wait tool. It blocks the tool call
// until every named thread (or, with no ids, every thread) leaves the
// pending/running/merging states, honoring the params timeout, the tool
// call's own context cancellation, and a message from the user.
//
// Deliberately absent from the default AllowedTools set (see
// internal/config's allToolNames) now that a thread's completion is
// delivered on its own through the completion inbox: the tool still
// exists, and buildTools still constructs and offers it, for the "wait
// for several threads to settle together" case an agent config can opt
// into explicitly.
func NewThreadWaitTool(manager ThreadManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ThreadWaitToolName,
		renderToolDescription(threadWaitDescriptionTpl),
		func(ctx context.Context, params ThreadWaitParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var timeout time.Duration
			if params.TimeoutSeconds > 0 {
				timeout = time.Duration(params.TimeoutSeconds) * time.Second
			}

			// A wait is the one thing an agent does that is not work: the
			// turn stays open while nothing happens, so a message typed
			// meanwhile would sit in the queue until the threads settle.
			// Cutting the wait short hands the turn back, the user is
			// answered now, and the threads report themselves through the
			// completion inbox as they always would.
			waitCtx, cancelWait := context.WithCancel(ctx)
			defer cancelWait()

			interrupted := make(chan struct{})
			if userInput := WaitForUserInput(ctx); userInput != nil {
				go func() {
					select {
					case <-userInput:
						close(interrupted)
						cancelWait()
					case <-waitCtx.Done():
					}
				}()
			}

			err := manager.Wait(waitCtx, params.IDs, timeout)
			if err == nil {
				return fantasy.NewTextResponse("All named threads have settled."), nil
			}

			select {
			case <-interrupted:
				// Report what is still running so the model can say
				// something useful about it, and pick the thread back up
				// later without another wait.
				return fantasy.NewTextResponse(interruptedWaitReport(ctx, manager, params.IDs)), nil
			default:
			}
			return fantasy.NewTextErrorResponse(err.Error()), nil
		},
	)
}

// interruptedWaitReport describes the wait that was cut short, naming the
// threads still going. A failure to list them is not worth surfacing as a
// tool error — the wait ending early is the news, and the model can ask for
// thread status itself.
func interruptedWaitReport(ctx context.Context, manager ThreadManager, ids []string) string {
	const lead = "Stopped waiting: the user sent a message. Answer them now; " +
		"the threads keep running and report when they finish."

	threads, err := manager.List(ctx)
	if err != nil {
		return lead
	}

	var active []string
	for _, t := range threads {
		if !slices.Contains(activeThreadStatuses, t.Status) {
			continue
		}
		if len(ids) > 0 && !slices.Contains(ids, t.ID) && !slices.Contains(ids, t.Name) {
			continue
		}
		name := t.Name
		if name == "" {
			name = t.ID
		}
		active = append(active, fmt.Sprintf("%s (%s)", name, t.Status))
	}
	if len(active) == 0 {
		return lead
	}
	return lead + " Still going: " + strings.Join(active, ", ") + "."
}
