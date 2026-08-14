package thread

import (
	"context"
	"time"

	"github.com/rave-soft/braid/internal/agent/tools"
)

// agentToolManager adapts a [Manager] to [tools.ThreadManager], the
// interface the thread_* agent tools are built against. It exists because
// internal/agent/tools cannot import internal/thread (internal/thread
// imports internal/app, which imports internal/agent, which imports
// internal/agent/tools — a cycle), so the tool-facing types there are
// declared independently and this type converts between the two.
type agentToolManager struct {
	m *Manager
}

// AsAgentToolManager returns m adapted to [tools.ThreadManager], for
// wiring into agent.CoordinatorOptions.
func AsAgentToolManager(m *Manager) tools.ThreadManager {
	return &agentToolManager{m: m}
}

func (a *agentToolManager) Create(ctx context.Context, args tools.ThreadCreateArgs) (tools.ThreadInfo, error) {
	st, err := a.m.Create(ctx, CreateArgs{
		Name:            args.Name,
		Goal:            args.Goal,
		BaseBranch:      args.BaseBranch,
		MergePolicy:     MergePolicy(args.MergePolicy),
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

func (a *agentToolManager) Get(ctx context.Context, idOrName string) (tools.ThreadInfo, error) {
	st, err := a.m.Get(ctx, idOrName)
	if err != nil {
		return tools.ThreadInfo{}, err
	}
	return toToolInfo(st), nil
}

func (a *agentToolManager) Send(ctx context.Context, idOrName, message string) error {
	return a.m.Send(ctx, idOrName, message)
}

func (a *agentToolManager) Wait(ctx context.Context, ids []string, timeout time.Duration) error {
	return a.m.Wait(ctx, ids, timeout)
}

func (a *agentToolManager) Merge(ctx context.Context, idOrName string) (tools.ThreadInfo, error) {
	st, err := a.m.Merge(ctx, idOrName)
	if err != nil {
		return tools.ThreadInfo{}, err
	}
	return toToolInfo(st), nil
}

func (a *agentToolManager) Remove(ctx context.Context, idOrName string, force, deleteBranch bool) error {
	return a.m.Remove(ctx, idOrName, force, deleteBranch)
}

func toToolInfo(st Thread) tools.ThreadInfo {
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
