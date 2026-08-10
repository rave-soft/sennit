package strand

import "github.com/rave-soft/braid/internal/proto"

// ToProto converts st to its wire representation. workspaceID is the
// strand's currently-spawned runtime workspace ID (see
// [Manager.WorkspaceID]), threaded through explicitly because it is
// manager-runtime state, not a column on the Strand row itself.
func ToProto(st Strand, workspaceID string) proto.Strand {
	return proto.Strand{
		ID:            st.ID,
		Name:          st.Name,
		Goal:          st.Goal,
		BaseBranch:    st.BaseBranch,
		Branch:        st.Branch,
		WorktreePath:  st.WorktreePath,
		WorkspaceID:   workspaceID,
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

// EventToProto converts a Manager lifecycle event to its wire form.
func EventToProto(e Event, workspaceID string) proto.StrandEvent {
	return proto.StrandEvent{
		Type:   proto.StrandEventType(e.Type),
		Strand: ToProto(e.Strand, workspaceID),
	}
}

// ToProto is a convenience wrapper for handlers/event bridges that only
// have a Manager and a Strand and want the correct WorkspaceID filled in
// without a separate m.WorkspaceID(id) call.
func (m *Manager) ToProto(st Strand) proto.Strand {
	return ToProto(st, m.WorkspaceID(st.ID))
}

// EventToProto is the Manager-bound counterpart of the package-level
// EventToProto, for the same reason.
func (m *Manager) EventToProto(e Event) proto.StrandEvent {
	return EventToProto(e, m.WorkspaceID(e.Strand.ID))
}

// FromProto converts a wire Strand back to its domain representation.
// WorkspaceID has no field on Strand (it's manager runtime state, not a
// persisted column — see ToProto) and is dropped; callers that need it
// read proto.Strand.WorkspaceID directly before converting.
func FromProto(s proto.Strand) Strand {
	return Strand{
		ID:            s.ID,
		Name:          s.Name,
		Goal:          s.Goal,
		BaseBranch:    s.BaseBranch,
		Branch:        s.Branch,
		WorktreePath:  s.WorktreePath,
		SessionID:     s.SessionID,
		Status:        Status(s.Status),
		MergePolicy:   MergePolicy(s.MergePolicy),
		ResultSummary: s.ResultSummary,
		Error:         s.Error,
		CreatedAt:     s.CreatedAt,
		UpdatedAt:     s.UpdatedAt,
		CompletedAt:   s.CompletedAt,
	}
}

// EventFromProto converts a wire StrandEvent back to a domain Event.
func EventFromProto(e proto.StrandEvent) Event {
	return Event{Type: EventType(e.Type), Strand: FromProto(e.Strand)}
}
