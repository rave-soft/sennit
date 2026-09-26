package workspace

import (
	"context"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
)

// PendingPrompts is every permission and question request currently
// awaiting an answer, across this workspace and every delegation live
// under it. A request is announced to subscribers exactly once, when it
// is raised (permission.Service.ActiveRequest's own doc comment), so a
// client that connects afterward has no way to learn about one already
// outstanding except by asking for this snapshot (CLIENT-SERVER.md, PR
// 1.4's Snapshot RPC).
type PendingPrompts struct {
	Permissions []permission.PermissionRequest
	Questions   []question.Request
}

// PendingPromptsReader is class U (CLIENT-SERVER.md's method-class
// table): unlike the class-C getters ClientState folds into one cached
// snapshot, a pending-prompts read has to reach every live delegation's
// own permission/question service (the same set forwardPermissions/
// forwardQuestions relay from) each time, so it stays an ordinary
// request/response call rather than something a cache can answer.
type PendingPromptsReader interface {
	PendingPrompts(ctx context.Context) (PendingPrompts, error)
}
