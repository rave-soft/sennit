package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// defaultIdlePollInterval is how often the idle monitor re-checks
// busyness when Options.IdlePollInterval is unset.
const defaultIdlePollInterval = 5 * time.Second

// idleClientCounter is the one piece of busyness the workspace interface
// itself doesn't carry: whether any frontend currently has this daemon's
// gRPC server open (CLIENT-SERVER.md, PR 2.1's "нет клиентов"). *grpcws.
// Server satisfies it via ClientCount.
type idleClientCounter interface {
	ClientCount() int
}

// excludingClientCounter adapts *grpcws.Server's ClientCountExcluding to
// idleClientCounter for the Shutdown RPC's OnlyIfIdle check (see
// daemon.go's shutdownHandler): the caller making that very RPC has an
// open connection of its own, and must not count as the "client" that
// makes the daemon look busy to itself.
type excludingClientCounter struct {
	server  *grpcws.Server
	exclude string
}

func (c *excludingClientCounter) ClientCount() int {
	return c.server.ClientCountExcluding(c.exclude)
}

// idleBusyCheck answers, on demand, whether the daemon has anything to do
// right now. Every source it consults is read-only and already exposed on
// workspace.Workspace/grpcws.Server for other reasons (the sidebar, `sennit
// ps`, the lease manager) -- this just ORs them together the way
// CLIENT-SERVER.md's PR 2.1 idle condition and its review point 4 (pending
// permissions AND questions, not just permissions) spell out.
type idleBusyCheck struct {
	ws      workspace.Workspace
	clients idleClientCounter
}

// busy reports whether the daemon currently counts as busy, and why, for
// the idle monitor's Debug log. A read that fails (ListThreads/ListTasks/
// PendingPrompts erroring, e.g. because the workspace is already shutting
// down) is treated as busy rather than idle: an idle monitor that cannot
// find out what is running must not guess "nothing", the same reasoning
// AGENTS.md gives for a bool-returning wrapper over something that can
// fail -- the caller here is "should I exit", and answering that on
// missing information would risk killing a daemon mid-turn.
func (c *idleBusyCheck) busy(ctx context.Context) (bool, string) {
	if n := c.clients.ClientCount(); n > 0 {
		return true, fmt.Sprintf("%d connected client(s)", n)
	}

	activity := c.ws.AgentActivity()
	if len(activity.BusySessions) > 0 {
		return true, fmt.Sprintf("%d busy session(s)", len(activity.BusySessions))
	}

	if counts := c.ws.BackgroundJobCounts(); counts.Active > 0 {
		return true, fmt.Sprintf("%d active background shell(s)", counts.Active)
	}

	if c.ws.SupportsThreads() {
		threads, err := c.ws.ListThreads(ctx)
		if err != nil {
			return true, fmt.Sprintf("could not list threads: %v", err)
		}
		for _, th := range threads {
			if !proto.ThreadStatus(th.Status).Terminal() {
				return true, fmt.Sprintf("thread %s is %s", th.ID, th.Status)
			}
		}
	}

	if c.ws.SupportsTasks() {
		tasks, err := c.ws.ListTasks(ctx)
		if err != nil {
			return true, fmt.Sprintf("could not list tasks: %v", err)
		}
		for _, tk := range tasks {
			if !proto.ThreadStatus(tk.Status).Terminal() {
				return true, fmt.Sprintf("task %s is %s", tk.ID, tk.Status)
			}
		}
	}

	prompts, err := c.ws.PendingPrompts(ctx)
	if err != nil {
		return true, fmt.Sprintf("could not read pending prompts: %v", err)
	}
	if len(prompts.Permissions) > 0 {
		return true, fmt.Sprintf("%d pending permission request(s)", len(prompts.Permissions))
	}
	if len(prompts.Questions) > 0 {
		return true, fmt.Sprintf("%d pending question(s)", len(prompts.Questions))
	}

	return false, ""
}

// runIdleMonitor polls check every pollInterval and calls onIdle once
// nothing has reported busy for a continuous span of timeout. It returns
// when ctx is done. timeout <= 0 disables idle exit entirely (the "0 or
// negative" semantics config.DaemonOptions.EffectiveIdleTimeout
// documents): the monitor still runs, logging at Debug, but never calls
// onIdle.
//
// idleSince is local to this call, not persisted: a monitor that starts
// mid-run (there is only one, for this daemon's whole lifetime) begins
// counting from its own first idle observation, never from some assumed
// start-of-day idleness.
func runIdleMonitor(ctx context.Context, check *idleBusyCheck, timeout, pollInterval time.Duration, onIdle func()) {
	if pollInterval <= 0 {
		pollInterval = defaultIdlePollInterval
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var idleSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		busy, reason := check.busy(ctx)
		if busy {
			if !idleSince.IsZero() {
				idleSince = time.Time{}
			}
			slog.Debug("Daemon staying up", "reason", reason)
			continue
		}

		if timeout <= 0 {
			slog.Debug("Daemon idle but idle exit is disabled", "idle_timeout", timeout)
			continue
		}

		if idleSince.IsZero() {
			idleSince = time.Now()
			continue
		}
		if time.Since(idleSince) >= timeout {
			slog.Info("Daemon exiting after idle timeout", "idle_timeout", timeout)
			onIdle()
			return
		}
	}
}
