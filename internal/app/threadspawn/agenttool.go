package threadspawn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/thread"
)

// agentToolManager adapts a *thread.Manager to tools.ThreadManager, the
// interface the thread_* agent tools are built against. It exists because
// internal/agent/tools cannot import internal/thread (internal/thread
// imports internal/app, which imports internal/agent, which imports
// internal/agent/tools — a cycle), so the tool-facing types there are
// declared independently and this type converts between the two.
type agentToolManager struct {
	m *thread.Manager
}

// AsAgentToolManager returns m adapted to tools.ThreadManager, for
// wiring into agent.CoordinatorOptions.
func AsAgentToolManager(m *thread.Manager) tools.ThreadManager {
	return &agentToolManager{m: m}
}

func (a *agentToolManager) Create(ctx context.Context, args tools.ThreadCreateArgs) (tools.ThreadInfo, error) {
	st, err := a.m.Create(ctx, thread.CreateArgs{
		Name:            args.Name,
		Goal:            args.Goal,
		BaseBranch:      args.BaseBranch,
		MergePolicy:     thread.MergePolicy(args.MergePolicy),
		ParentSessionID: args.ParentSessionID,
	})
	if err != nil {
		return tools.ThreadInfo{}, err
	}
	return toToolInfo(st), nil
}

func (a *agentToolManager) List(ctx context.Context) ([]tools.ThreadInfo, error) {
	threads, err := a.m.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tools.ThreadInfo, len(threads))
	for i, st := range threads {
		out[i] = toToolInfo(st)
	}
	return out, nil
}

// toolErr translates this package's sentinels into the tools package's,
// so a tool can recognize "no such thread" without importing the thread
// domain (see tools.ThreadManager).
func toolErr(err error) error {
	if errors.Is(err, thread.ErrNotFound) {
		return fmt.Errorf("%w", tools.ErrThreadNotFound)
	}
	return err
}

func (a *agentToolManager) Get(ctx context.Context, idOrName string) (tools.ThreadInfo, error) {
	st, err := a.m.Get(ctx, idOrName)
	if err != nil {
		return tools.ThreadInfo{}, toolErr(err)
	}
	return toToolInfo(st), nil
}

func (a *agentToolManager) Send(ctx context.Context, idOrName, message string) (tools.SendOutcome, error) {
	disp, err := a.m.Send(ctx, idOrName, message)
	if err != nil {
		return tools.SendOutcome{}, err
	}
	return toToolSendOutcome(disp), nil
}

func (a *agentToolManager) Cancel(ctx context.Context, idOrName, reason string) error {
	return toolErr(a.m.Cancel(ctx, idOrName, reason))
}

func (a *agentToolManager) Wait(ctx context.Context, ids []string, timeout time.Duration) error {
	return a.m.Wait(ctx, ids, timeout)
}

func (a *agentToolManager) Merge(ctx context.Context, idOrName string) (tools.ThreadInfo, error) {
	st, err := a.m.Merge(ctx, idOrName)
	if err != nil {
		return tools.ThreadInfo{}, toolErr(err)
	}
	return toToolInfo(st), nil
}

func (a *agentToolManager) Remove(ctx context.Context, idOrName string, force, deleteBranch bool) error {
	return a.m.Remove(ctx, idOrName, force, deleteBranch)
}

// toToolSendOutcome maps the domain's send disposition to the tools
// package's identical spelling of it, the same one-to-one seam conversion
// toToolInfo does for a thread — shared with the task adapter, since a
// task's send reports the same two possibilities.
func toToolSendOutcome(d thread.SendDisposition) tools.SendOutcome {
	return tools.SendOutcome{
		Queued:  d.Queued,
		Ahead:   d.Ahead,
		Resumed: d.Resumed,
	}
}

func toToolInfo(st thread.Thread) tools.ThreadInfo {
	return tools.ThreadInfo{
		ID:            st.ID,
		Name:          st.Name,
		Goal:          st.Goal,
		BaseBranch:    st.BaseBranch,
		Branch:        st.Branch,
		WorktreePath:  st.WorktreePath,
		SessionID:     st.SessionID,
		Status:        string(st.Status),
		MergePolicy:   string(st.MergePolicy),
		ResultSummary: st.ResultSummary,
		Error:         st.Error,
		CreatedAt:     st.CreatedAt,
		UpdatedAt:     st.UpdatedAt,
		CompletedAt:   st.CompletedAt,
	}
}
