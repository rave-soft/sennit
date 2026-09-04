package appws

import (
	"context"
	"errors"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/rave-soft/sennit/internal/workspace"
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
	workspace.Workspace
	mgr *thread.Manager
	// parent is the workspace this thread was attached from -- the one
	// whose screen the user came in from. Kept for permission answers;
	// see PermissionGrant.
	parent *AppWorkspace
	// threadID and sessionID identify the one delegation, and the one
	// session within it, this redirect applies to.
	threadID  string
	sessionID string
}

// PermissionGrant, PermissionGrantPersistent and PermissionDeny answer on
// the thread's own workspace first and fall back to the parent's.
//
// While the user is drilled into a thread, every event is routed to that
// thread's UI -- including a permission raised by the parent workspace
// behind it, whose agent goes on working while the thread screen is up.
// Answering it here reached only the thread's own permission service,
// which is not holding that request, so the answer went nowhere: the
// prompt stayed on screen and every click reported "permission response
// was not accepted", with no way to dismiss it or get on with the work.
//
// Trying both is safe for the reason answerPermission spells out: a
// service that is not holding the request does nothing at all.
func (w *attachedThreadWorkspace) PermissionGrant(perm permission.PermissionRequest) bool {
	return answerPermission(
		func() bool { return w.Workspace.PermissionGrant(perm) },
		w.parentAttempt(func(p *AppWorkspace) bool { return p.PermissionGrant(perm) }),
	)
}

func (w *attachedThreadWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) bool {
	return answerPermission(
		func() bool { return w.Workspace.PermissionGrantPersistent(perm) },
		w.parentAttempt(func(p *AppWorkspace) bool { return p.PermissionGrantPersistent(perm) }),
	)
}

func (w *attachedThreadWorkspace) PermissionDeny(perm permission.PermissionRequest) bool {
	return answerPermission(
		func() bool { return w.Workspace.PermissionDeny(perm) },
		w.parentAttempt(func(p *AppWorkspace) bool { return p.PermissionDeny(perm) }),
	)
}

// QuestionAnswer and QuestionCancel answer on the thread's own workspace
// first and fall back to the parent's, for the same reason as
// PermissionGrant above: while the user is drilled into this thread, a
// question raised by the parent workspace behind it is relayed onto this
// screen too, and answering it here must be able to reach the service
// that is actually holding it.
func (w *attachedThreadWorkspace) QuestionAnswer(batchID string, responses []question.Answer) bool {
	return answerPermission(
		func() bool { return w.Workspace.QuestionAnswer(batchID, responses) },
		w.parentAttempt(func(p *AppWorkspace) bool { return p.QuestionAnswer(batchID, responses) }),
	)
}

func (w *attachedThreadWorkspace) QuestionCancel() bool {
	return answerPermission(
		func() bool { return w.Workspace.QuestionCancel() },
		w.parentAttempt(func(p *AppWorkspace) bool { return p.QuestionCancel() }),
	)
}

// parentAttempt wraps an answer against the parent workspace, or nil when
// there is none to fall back to. Going through the parent workspace rather
// than straight to its permission service keeps its own routing (a
// delegation's prompt relayed into it) in play.
func (w *attachedThreadWorkspace) parentAttempt(answer func(*AppWorkspace) bool) func() bool {
	if w.parent == nil {
		return nil
	}
	return func() bool { return answer(w.parent) }
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
func (w *attachedThreadWorkspace) SubscribeWith(send func(any)) func() {
	sub, ok := w.Workspace.(interface {
		SubscribeWith(func(any)) func()
	})
	if !ok {
		return func() {}
	}
	return sub.SubscribeWith(send)
}

func (w *attachedThreadWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]workspace.SessionFile, error) {
	preparer, ok := w.Workspace.(workspace.SessionChangePreparer)
	if !ok {
		return nil, errors.New("session change preparer is unavailable")
	}
	return preparer.PrepareSessionChanges(ctx, sessionID)
}

// ApplySessionModel is refused for the thread's own session: a thread runs
// the model its parent workspace is running, and it is handed that model
// at spawn (see app.BootstrapOptions.PreferredModel). The pin on this
// session records whichever model the thread happened to run on before —
// possibly several restarts and model switches ago — so honoring it here
// would quietly undo the inheritance the moment the person opened the
// thread's screen, which is the one thing they do before typing into it.
//
// Reported as "not switched" rather than as an error: nothing failed, and
// the caller only uses the bool to decide whether to re-probe the memoized
// model state, which has not changed.
//
// Any other session reachable from here is not the delegation and passes
// through, matching AgentRun.
func (w *attachedThreadWorkspace) ApplySessionModel(ctx context.Context, sessionID string) (bool, error) {
	if sessionID != w.sessionID {
		return w.Workspace.ApplySessionModel(ctx, sessionID)
	}
	return false, nil
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
