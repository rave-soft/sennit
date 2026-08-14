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

// ThreadEventToProto is the Manager-bound counterpart of the package-level
// EventToProto, for the same reason.
func ThreadEventToProto(m *thread.Manager, e thread.Event) proto.ThreadEvent {
	return EventToProto(e, m.WorkspaceID(e.Thread.ID))
}

// FromProto converts a wire Thread back to its domain representation.
// WorkspaceID has no field on Thread (it's manager runtime state, not a
// persisted column — see ToProto) and is dropped; callers that need it
// read proto.Thread.WorkspaceID directly before converting.
func FromProto(s proto.Thread) thread.Thread {
	return thread.Thread{
		Delegation: thread.Delegation{
			ID:              s.ID,
			Name:            s.Name,
			Goal:            s.Goal,
			SessionID:       s.SessionID,
			Status:          thread.Status(s.Status),
			Kind:            thread.Kind(s.Kind),
			ResultSummary:   s.ResultSummary,
			Error:           s.Error,
			CreatedAt:       s.CreatedAt,
			UpdatedAt:       s.UpdatedAt,
			CompletedAt:     s.CompletedAt,
			ParentSessionID: s.ParentSessionID,
		},
		BaseBranch:   s.BaseBranch,
		Branch:       s.Branch,
		WorktreePath: s.WorktreePath,
		MergePolicy:  thread.MergePolicy(s.MergePolicy),
	}
}

// EventFromProto converts a wire ThreadEvent back to a domain Event.
func EventFromProto(e proto.ThreadEvent) thread.Event {
	return thread.Event{Type: thread.EventType(e.Type), Thread: FromProto(e.Thread)}
}
