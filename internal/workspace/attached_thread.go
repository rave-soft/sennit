package workspace

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/thread"
)

// attachedThreadWorkspace is the workspace a caller drives after drilling
// into a live thread (see AppWorkspace.AttachThread). It is the thread's
// own AppWorkspace in every respect but one: a turn the person starts in
// the thread's own session is dispatched through the thread Manager
// instead of straight into the thread's coordinator.
//
// That one redirect is the difference between a thread that knows what is
// happening inside it and one that does not. Typing here used to reach the
// coordinator directly, so the Manager never learned a turn had started:
// the thread sat at whatever status it had (idle, right after being
// revived) while its agent worked, and the terminal event was dropped on
// arrival — an untracked run carries no RunID, and the Manager matches
// completions by RunID. A thread revived and driven by hand could
// therefore never settle, never merge, and never report to its parent
// again.
//
// Every other session reachable from here — a sub-agent's, another
// session in the thread's own database — is not the delegation, so it
// passes straight through.
type attachedThreadWorkspace struct {
	// Workspace is the thread's own AppWorkspace: everything this type
	// does not override is its behavior, unchanged.
	Workspace
	mgr *thread.Manager
	// threadID and sessionID identify the one delegation, and the one
	// session within it, this redirect applies to.
	threadID  string
	sessionID string
}

// SubscribeWith forwards to the wrapped workspace's own subscription.
//
// It has to be spelled out. The embedded field is the Workspace
// *interface*, and SubscribeWith is not part of it — it is a concrete
// method on AppWorkspace — so nothing is promoted and this wrapper does
// not satisfy the subscriber interface the TUI type-asserts for when
// attaching (see the router's handleThreadAttached). That assertion
// failing is silent: the attach succeeds and the thread's screen simply
// never receives an event again. Its chat stopped growing as its agent
// worked, and only leaving and re-entering showed what had happened,
// because that re-reads the messages instead of being told about them.
//
// The type assertion is kept rather than widened to the Workspace
// interface for the reason SubscribeWith is not on it: this is a second,
// independently stoppable subscription that only a workspace backed by a
// real App can offer.
func (w *attachedThreadWorkspace) SubscribeWith(send func(tea.Msg)) func() {
	sub, ok := w.Workspace.(interface {
		SubscribeWith(func(tea.Msg)) func()
	})
	if !ok {
		return func() {}
	}
	return sub.SubscribeWith(send)
}

// AgentRun routes a turn in the thread's own session through the Manager,
// which records it as running, owns its completion, and rests the thread
// at idle when it ends (see thread.Manager.RunFromPerson).
//
// It keeps AgentRun's contract: the Manager returns once the turn has been
// accepted — folded into the turn already in flight, or started as a new
// one — not once it completes.
func (w *attachedThreadWorkspace) AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	if sessionID != w.sessionID {
		return w.Workspace.AgentRun(ctx, sessionID, prompt, attachments...)
	}
	_, err := w.mgr.RunFromPerson(ctx, w.threadID, prompt, toThreadAttachments(attachments))
	return err
}

// toThreadAttachments maps attachments onto the thread domain's own DTO;
// threadspawn's coordinator adapter maps them back at the far end. The
// domain declares its own type rather than naming message.Attachment for
// the same reason it declares its own Coordinator port — see
// thread.Attachment.
func toThreadAttachments(in []message.Attachment) []thread.Attachment {
	if len(in) == 0 {
		return nil
	}
	out := make([]thread.Attachment, 0, len(in))
	for _, a := range in {
		out = append(out, thread.Attachment{
			FilePath: a.FilePath,
			FileName: a.FileName,
			MimeType: a.MimeType,
			Content:  a.Content,
		})
	}
	return out
}
