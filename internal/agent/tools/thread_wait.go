package tools

import (
	"context"
	_ "embed"
	"errors"
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
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" description:"Give up after this many seconds (default: 600; negative for no timeout)"`
}

// defaultThreadWaitTimeout bounds a wait that named no timeout of its own.
// An unbounded wait rests on two things ending it — the user typing, and
// the turn being canceled — and a thread that hangs with neither happening
// parks the turn for as long as the process lives. Ten minutes is longer
// than the threads worth waiting on together take to settle, and short
// enough that a stuck one surfaces as a report rather than as silence.
const defaultThreadWaitTimeout = 10 * time.Minute

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
			// Zero is "said nothing", not "wait forever": bound it. A
			// negative value is the explicit opt-out for a caller that
			// really does want to block indefinitely.
			timeout := defaultThreadWaitTimeout
			switch {
			case params.TimeoutSeconds > 0:
				timeout = time.Duration(params.TimeoutSeconds) * time.Second
			case params.TimeoutSeconds < 0:
				timeout = 0
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
			// A timeout now happens by default rather than only when the
			// caller asked for one, so say what it means. Left an error
			// (the wait did not get what it waited for), but with the
			// state the model needs to decide something other than
			// waiting again.
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return fantasy.NewTextErrorResponse(timedOutWaitReport(ctx, manager, params.IDs, timeout)), nil
			}
			return fantasy.NewTextErrorResponse(err.Error()), nil
		},
	)
}

// interruptedWaitReport describes the wait that was cut short, naming the
// threads still going.
func interruptedWaitReport(ctx context.Context, manager ThreadManager, ids []string) string {
	const lead = "Stopped waiting: the user sent a message. Answer them now; " +
		"the threads keep running and report when they finish."

	return withStillGoing(lead, activeThreadNames(ctx, manager, ids))
}

// timedOutWaitReport describes a wait that ran out of time. The threads are
// still running and still report themselves, so the one thing this must not
// leave sounding reasonable is calling the same wait again and burning
// another timeout on it.
func timedOutWaitReport(ctx context.Context, manager ThreadManager, ids []string, timeout time.Duration) string {
	lead := fmt.Sprintf("Waited %s and gave up; the threads did not all settle. "+
		"They keep running and report when they finish, so end your turn rather "+
		"than waiting again — unless you genuinely cannot continue until they land, "+
		"in which case wait again with a longer timeout_seconds.", timeout)
	return withStillGoing(lead, activeThreadNames(ctx, manager, ids))
}

// activeThreadNames names the threads in scope that are still going. A
// failure to list them is not worth surfacing as a tool error — the wait
// ending is the news, and the model can ask for thread status itself.
func activeThreadNames(ctx context.Context, manager ThreadManager, ids []string) []string {
	threads, err := manager.List(ctx)
	if err != nil {
		return nil
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
	return active
}

func withStillGoing(lead string, active []string) string {
	if len(active) == 0 {
		return lead
	}
	return lead + " Still going: " + strings.Join(active, ", ") + "."
}
