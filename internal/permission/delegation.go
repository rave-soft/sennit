package permission

import "context"

// DelegationRef identifies the background delegation (see
// internal/thread.Delegation) whose run raised a permission request, if
// any. It is a self-contained value rather than an internal/thread type:
// this package must not import internal/thread (or internal/agent), so a
// caller that knows about delegations — internal/thread's task dispatch —
// copies the identity it cares about into this shape instead of handing
// over a domain object. The zero value means "the visible turn asked",
// which is the only value every request had before this existed.
type DelegationRef struct {
	ID   string
	Name string
	Kind string
}

// delegationContextKey is the unexported context key WithDelegation uses.
type delegationContextKey struct{}

// WithDelegation returns ctx tagged with ref, so a [Service.Request] made
// under it is attributed to that delegation rather than to the visible
// turn. Mirrors internal/agent's WithRunID/RunIDFromContext.
func WithDelegation(ctx context.Context, ref DelegationRef) context.Context {
	return context.WithValue(ctx, delegationContextKey{}, ref)
}

// DelegationFromContext returns the DelegationRef set by [WithDelegation],
// or the zero value if none was set. Safe to call on any context.
func DelegationFromContext(ctx context.Context) DelegationRef {
	if v, ok := ctx.Value(delegationContextKey{}).(DelegationRef); ok {
		return v
	}
	return DelegationRef{}
}
