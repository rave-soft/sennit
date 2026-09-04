package appws

import (
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
)

// -- Permissions --

// permissionsFor resolves the service actually holding perm, which is not
// always this workspace's own: a thread's prompts are raised inside its
// isolated workspace and relayed here for display (see
// lifecycle.forwardPermissions), so the answer has to travel back to the
// service that is still blocking on it. Falls back to this workspace's own
// service for everything else — the user's own turn, and tasks, which run
// in this very App.
func (w *AppWorkspace) permissionsFor(perm permission.PermissionRequest) []permission.Resolver {
	own := w.app.Permissions()
	if perm.Delegation.ID == "" {
		return []permission.Resolver{own}
	}
	mgr, ok := w.threadManager()
	if !ok {
		return []permission.Resolver{own}
	}
	if svc := mgr.PermissionsFor(perm.Delegation.ID); svc != nil {
		return []permission.Resolver{svc, own}
	}
	return []permission.Resolver{own}
}

// answerPermission hands perm to each candidate service in turn until one
// accepts it.
//
// Routing has to guess, and a wrong guess used to be fatal to the prompt.
// The tag a request carries is the delegation whose run raised it, which
// is not the same question as "which permission service is blocked on
// this id": a thread's runtime can have been replaced since the prompt was
// published, and the screen the answer is given on is not necessarily the
// workspace the prompt came from -- while the user is drilled into a
// thread, every event is routed to that thread's UI, including prompts
// raised by the parent workspace behind it. Answering the wrong service
// leaves the right one blocked forever with its dialog still on screen,
// and every further click reports "permission response was not accepted".
//
// Trying the others is safe rather than merely convenient: a service
// resolves a request only if it wins the take of that id from its own
// pending map (see permission.resolve), so a service that is not holding
// the request does nothing at all and says so. Order still matters --
// the routed service is asked first -- but only for cost, not
// correctness.
func answerPermission(attempts ...func() bool) bool {
	for _, attempt := range attempts {
		if attempt != nil && attempt() {
			return true
		}
	}
	return false
}

// serviceAttempts adapts candidate services into answerPermission attempts.
func serviceAttempts(services []permission.Resolver, answer func(permission.Resolver) bool) []func() bool {
	attempts := make([]func() bool, 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		attempts = append(attempts, func() bool { return answer(svc) })
	}
	return attempts
}

func (w *AppWorkspace) PermissionGrant(perm permission.PermissionRequest) bool {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Resolver) bool { return s.Grant(perm) })...)
}

func (w *AppWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) bool {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Resolver) bool { return s.GrantPersistent(perm) })...)
}

func (w *AppWorkspace) PermissionDeny(perm permission.PermissionRequest) bool {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Resolver) bool { return s.Deny(perm) })...)
}

func (w *AppWorkspace) PermissionSkipRequests() bool {
	return w.app.Permissions().SkipRequests()
}

func (w *AppWorkspace) PermissionSetSkipRequests(skip bool) {
	w.app.SetPermissionsSkip(skip)
}

// -- Questions --

// questionServices returns every question.Service this workspace's answer
// might belong to: its own, then one per live delegation. A batch ID
// carries no delegation tag (see Manager.QuestionServices), so unlike
// permissionsFor this cannot route straight to the right one — it tries
// them all, safe for the reason answerPermission's doc spells out: a
// service not holding the given batch ID does nothing at all.
func (w *AppWorkspace) questionServices() []question.Service {
	own := w.app.Questions
	mgr, ok := w.threadManager()
	if !ok {
		return []question.Service{own}
	}
	services := append([]question.Service{own}, mgr.QuestionServices()...)
	return services
}

func questionServiceAttempts(services []question.Service, answer func(question.Service) bool) []func() bool {
	attempts := make([]func() bool, 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		attempts = append(attempts, func() bool { return answer(svc) })
	}
	return attempts
}

func (w *AppWorkspace) QuestionAnswer(batchID string, responses []question.Answer) bool {
	return answerPermission(questionServiceAttempts(w.questionServices(),
		func(s question.Service) bool { return s.Answer(batchID, responses) })...)
}

func (w *AppWorkspace) QuestionCancel() bool {
	return answerPermission(questionServiceAttempts(w.questionServices(),
		func(s question.Service) bool { return s.Cancel() })...)
}
