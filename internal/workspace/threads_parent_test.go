package workspace

import (
	"testing"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/stretchr/testify/require"
)

// TestAppWorkspace_CreateThreadCarriesParentSession pins the link a
// thread's completion travels back along. A thread reports to the session
// it was started from; created without one it has nobody to tell, and its
// result becomes discoverable only by going and looking — which is what
// happened for every thread created outside the model's own tool, since
// the request carried no parent field at all.
func TestAppWorkspace_CreateThreadCarriesParentSession(t *testing.T) {
	aw, mgr := newTestThreadAppWorkspace(t)

	created, err := aw.CreateThread(t.Context(), proto.CreateThreadRequest{
		Name:            "with-parent",
		Goal:            "do the thing",
		ParentSessionID: "parent-session",
	})
	require.NoError(t, err)
	require.Equal(t, "parent-session", mgr.ParentSessionIDForTest(created.ID),
		"the workspace must pass the parent through, or completions have nowhere to go")

	orphan, err := aw.CreateThread(t.Context(), proto.CreateThreadRequest{
		Name: "no-parent",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.Empty(t, mgr.ParentSessionIDForTest(orphan.ID),
		"a thread created outside a session still has nobody to report to")
}
