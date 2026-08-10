package strand

import (
	"testing"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/stretchr/testify/require"
)

func testStrand() Strand {
	return Strand{
		ID:            "strand-1",
		Name:          "alpha",
		Goal:          "implement the thing",
		BaseBranch:    "main",
		Branch:        "strand/alpha",
		WorktreePath:  "/tmp/repo-strands/alpha",
		SessionID:     "sess-1",
		Status:        StatusRunning,
		MergePolicy:   MergeAuto,
		ResultSummary: "",
		Error:         "",
		CreatedAt:     100,
		UpdatedAt:     200,
		CompletedAt:   0,
	}
}

func TestToProto(t *testing.T) {
	st := testStrand()

	got := ToProto(st, "ws-123")
	require.Equal(t, proto.Strand{
		ID:           st.ID,
		Name:         st.Name,
		Goal:         st.Goal,
		BaseBranch:   st.BaseBranch,
		Branch:       st.Branch,
		WorktreePath: st.WorktreePath,
		WorkspaceID:  "ws-123",
		SessionID:    st.SessionID,
		Status:       string(StatusRunning),
		MergePolicy:  string(MergeAuto),
		CreatedAt:    100,
		UpdatedAt:    200,
	}, got)

	// A non-spawned strand carries no WorkspaceID.
	require.Empty(t, ToProto(st, "").WorkspaceID)
}

func TestEventToProto(t *testing.T) {
	st := testStrand()
	ev := Event{Type: EventStatusChanged, Strand: st}

	got := EventToProto(ev, "ws-123")
	require.Equal(t, proto.StrandEventStatusChanged, got.Type)
	require.Equal(t, "ws-123", got.Strand.WorkspaceID)
	require.Equal(t, st.ID, got.Strand.ID)
}

func TestFromProto(t *testing.T) {
	st := testStrand()

	got := FromProto(ToProto(st, "ws-123"))
	require.Equal(t, st, got)
}

func TestEventFromProto(t *testing.T) {
	st := testStrand()
	ev := Event{Type: EventStatusChanged, Strand: st}

	got := EventFromProto(EventToProto(ev, "ws-123"))
	require.Equal(t, ev, got)
}

// TestManager_ToProto verifies the *Manager convenience methods fill in
// WorkspaceID from the manager's own runtime state, matching what a
// caller with only a Strand (not a workspace ID) would otherwise have to
// look up manually via Manager.WorkspaceID.
func TestManager_ToProto(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "nu", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	got := mgr.ToProto(st)
	require.Equal(t, st.WorktreePath, got.WorkspaceID)

	ev := Event{Type: EventCreated, Strand: st}
	gotEv := mgr.EventToProto(ev)
	require.Equal(t, proto.StrandEventCreated, gotEv.Type)
	require.Equal(t, st.WorktreePath, gotEv.Strand.WorkspaceID)

	require.NoError(t, mgr.Remove(t.Context(), st.ID, true, true))
	require.Empty(t, mgr.ToProto(st).WorkspaceID)
}
