package appws

import (
	"context"
	"errors"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/workspace"
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
//
// Semantics: the first attempt that resolves the request wins, reported
// as (true, nil). If none resolves, the result is (false, err) where err
// joins every error an attempt reported (nil if none did) — a resolver
// that simply did not hold the request is not an error, only one that
// could not be asked at all is.
func answerPermission(attempts ...func() (bool, error)) (bool, error) {
	var errs []error
	for _, attempt := range attempts {
		if attempt == nil {
			continue
		}
		ok, err := attempt()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, errors.Join(errs...)
}

// serviceAttempts adapts candidate services into answerPermission attempts.
func serviceAttempts(services []permission.Resolver, answer func(permission.Resolver) bool) []func() (bool, error) {
	attempts := make([]func() (bool, error), 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		attempts = append(attempts, func() (bool, error) { return answer(svc), nil })
	}
	return attempts
}

func (w *AppWorkspace) PermissionGrant(perm permission.PermissionRequest) (bool, error) {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Resolver) bool { return s.Grant(perm) })...)
}

func (w *AppWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) (bool, error) {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Resolver) bool { return s.GrantPersistent(perm) })...)
}

func (w *AppWorkspace) PermissionDeny(perm permission.PermissionRequest) (bool, error) {
	return answerPermission(serviceAttempts(w.permissionsFor(perm),
		func(s permission.Resolver) bool { return s.Deny(perm) })...)
}

func (w *AppWorkspace) PermissionSkipRequests() bool {
	return w.app.Permissions().SkipRequests()
}

func (w *AppWorkspace) PermissionSetSkipRequests(skip bool) error {
	w.app.SetPermissionsSkip(skip)
	return nil
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

func questionServiceAttempts(services []question.Service, answer func(question.Service) bool) []func() (bool, error) {
	attempts := make([]func() (bool, error), 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		attempts = append(attempts, func() (bool, error) { return answer(svc), nil })
	}
	return attempts
}

func (w *AppWorkspace) QuestionAnswer(batchID string, responses []question.Answer) (bool, error) {
	return answerPermission(questionServiceAttempts(w.questionServices(),
		func(s question.Service) bool { return s.Answer(batchID, responses) })...)
}

func (w *AppWorkspace) QuestionCancel(batchID string) (bool, error) {
	return answerPermission(questionServiceAttempts(w.questionServices(),
		func(s question.Service) bool { return s.Cancel(batchID) })...)
}

// -- Pending prompts --

// PendingPrompts collects every permission and question request currently
// awaiting an answer: this workspace's own two services, plus one per live
// delegation (threads and tasks alike) reached the same way permissionsFor/
// questionServices already do. ctx is unused — every lookup here is an
// in-memory read — but kept to match PendingPromptsReader's signature
// (every other read on a live delegation's services takes none either, so
// there is nothing to cancel).
func (w *AppWorkspace) PendingPrompts(context.Context) (workspace.PendingPrompts, error) {
	var out workspace.PendingPrompts

	permServices := []permission.Service{w.app.Permissions()}
	qServices := []question.Service{w.app.Questions}
	if mgr, ok := w.threadManager(); ok {
		permServices = append(permServices, mgr.PermissionServices()...)
		qServices = append(qServices, mgr.QuestionServices()...)
	}

	for _, svc := range permServices {
		if svc == nil {
			continue
		}
		if req, ok := svc.ActiveRequest(); ok {
			out.Permissions = append(out.Permissions, req)
		}
	}
	for _, svc := range qServices {
		if svc == nil {
			continue
		}
		if req, ok := svc.ActiveRequest(); ok {
			out.Questions = append(out.Questions, req)
		}
	}
	return out, nil
}
