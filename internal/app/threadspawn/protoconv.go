package threadspawn

import (
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
)

// ToProto converts st to its wire representation. workspaceID is the
// thread's currently-spawned runtime workspace ID (see
// thread.Manager.WorkspaceID), threaded through explicitly because it is
// manager-runtime state, not a column on the Thread row itself.
func ToProto(st thread.Thread, workspaceID string) proto.Thread {
	return proto.Thread{
		ID:              st.ID,
		Name:            st.Name,
		Goal:            st.Goal,
		BaseBranch:      st.BaseBranch,
		Branch:          st.Branch,
		WorktreePath:    st.WorktreePath,
		WorkspaceID:     workspaceID,
		SessionID:       st.SessionID,
		Status:          string(st.Status),
		Kind:            string(st.Kind),
		MergePolicy:     string(st.MergePolicy),
		ResultSummary:   st.ResultSummary,
		Error:           st.Error,
		CreatedAt:       st.CreatedAt,
		UpdatedAt:       st.UpdatedAt,
		CompletedAt:     st.CompletedAt,
		ParentSessionID: st.ParentSessionID,
	}
}

// EventToProto converts a thread lifecycle event to its wire form.
func EventToProto(e thread.Event, workspaceID string) proto.ThreadEvent {
	return proto.ThreadEvent{
		Type:   proto.ThreadEventType(e.Type),
		Thread: ToProto(e.Thread, workspaceID),
	}
}

// ThreadToProto is a convenience wrapper for handlers/event bridges that
// only have a Manager and a Thread and want the correct WorkspaceID filled
// in without a separate m.WorkspaceID(id) call.
func ThreadToProto(m *thread.Manager, st thread.Thread) proto.Thread {
	return ToProto(st, m.WorkspaceID(st.ID))
}
