package wsrpc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
)

// The methods below are Loopback's S/H coverage: the generator
// (zz_generated_loopback.go) only emits U/C methods, so these are
// hand-written. Unlike loopback_manual.go's earlier, plain-delegation
// version, every one of them now pushes its data through the same JSON
// codec the U/C methods use -- a real wire hop serializes these too, and
// PR 0.7's whole point is that anything that would break there breaks
// here first (CLIENT-SERVER.md, "PR 0.7"). Shutdown (X) stays a plain
// pass-through: it never crosses the wire at all. This file exists so
// *Loopback satisfies workspace.Workspace in full -- see the
// compile-time assertion below.
var _ workspace.Workspace = (*Loopback)(nil)

// jsonRoundTrip re-encodes v through encoding/json, panicking (naming
// what) if either half fails. Used for values that travel as plain data
// -- no error, no identity to preserve beyond their fields.
func jsonRoundTrip[T any](what string, v T) T {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("wsrpc: marshaling %s: %v", what, err))
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("wsrpc: decoding %s: %v", what, err))
	}
	return out
}

// roundTripError converts err to its wire shape (workspace.EncodeError),
// round-trips that through JSON, and converts it back
// (workspace.DecodeError) -- so errors.Is on a sentinel still succeeds
// after a value has gone through exactly what a real wire hop does to
// it. nil in, nil out.
func roundTripError(what string, err error) error {
	if err == nil {
		return nil
	}
	return workspace.DecodeError(jsonRoundTrip(what+" error", workspace.EncodeError(err)))
}

// codecEvent round-trips one event delivered to a Subscribe/SubscribeWith
// callback through Envelope -- EncodeEvent, a JSON hop, DecodeEvent --
// using the registry in events.go. A codec error panics naming ev's
// concrete type: Subscribe/SubscribeWith only ever receive one of the
// registry's types (appws.AppWorkspace.translateEvent's doc comment is
// the source of that invariant), so a panic here means the registry
// fell out of sync with translateEvent, exactly what
// TestTranslateEventOutputsAreRegistered in internal/workspace/appws
// exists to catch before it reaches this loopback.
func codecEvent(ev any) any {
	env, err := EncodeEvent(ev)
	if err != nil {
		panic(fmt.Sprintf("wsrpc: encoding event %T: %v", ev, err))
	}
	env = jsonRoundTrip(fmt.Sprintf("event envelope for %T", ev), env)
	decoded, err := DecodeEvent(env)
	if err != nil {
		panic(fmt.Sprintf("wsrpc: decoding event %q: %v", env.Type, err))
	}
	return decoded
}

// AgentRunShellCommand is class S. onProgress is called with plain
// strings, which need no codec of their own, so it passes straight
// through to inner; the response and a non-nil error each round-trip.
func (l *Loopback) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	resp, err := l.inner.AgentRunShellCommand(ctx, sessionID, command, termWidth, onProgress, isFirstMessage)
	return jsonRoundTrip("ShellCommandResponse", resp), roundTripError("AgentRunShellCommand", err)
}

// AgentRunStream is class S. opts round-trips on the way in; the
// returned channel is a new one this method owns, forwarding inner's
// events each round-tripped through JSON, so the channel itself (unlike
// a plain pass-through) never crosses back to the caller unwrapped.
func (l *Loopback) AgentRunStream(ctx context.Context, sessionID, prompt string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	inner, err := l.inner.AgentRunStream(ctx, sessionID, prompt, jsonRoundTrip("AgentRunOptions", opts))
	if err != nil {
		return nil, roundTripError("AgentRunStream", err)
	}
	out := make(chan workspace.AgentRunEvent)
	go func() {
		defer close(out)
		for {
			select {
			case ev, ok := <-inner:
				if !ok {
					return
				}
				select {
				case out <- jsonRoundTrip("AgentRunEvent", ev):
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Subscribe is class S. Every event send delivers, translateEvent's own
// doc comment guarantees is one of events.go's registered types, is
// round-tripped through the event codec before reaching the caller's
// send.
func (l *Loopback) Subscribe(send func(any)) {
	l.inner.Subscribe(func(ev any) { send(codecEvent(ev)) })
}

// SubscribeWith is class S; see Subscribe.
func (l *Loopback) SubscribeWith(send func(any)) func() {
	return l.inner.SubscribeWith(func(ev any) { send(codecEvent(ev)) })
}

// StartOAuth is class H. OAuthStartResult and a non-nil error
// round-trip directly; the returned OAuthFlow (a handle, not data) is
// wrapped so its own Wait keeps round-tripping the completion and error
// it eventually produces.
func (l *Loopback) StartOAuth(ctx context.Context, providerID, proxyURL string, forceNewAccount bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	result, flow, err := l.inner.StartOAuth(ctx, providerID, proxyURL, forceNewAccount)
	result = jsonRoundTrip("OAuthStartResult", result)
	err = roundTripError("StartOAuth", err)
	if flow == nil {
		return result, nil, err
	}
	return result, codecOAuthFlow{inner: flow}, err
}

// codecOAuthFlow wraps a workspace.OAuthFlow handle so a caller that
// waits on it through Loopback sees the same codec treatment StartOAuth
// itself gets.
type codecOAuthFlow struct {
	inner workspace.OAuthFlow
}

func (f codecOAuthFlow) Wait(ctx context.Context) (workspace.OAuthCompletion, error) {
	completion, err := f.inner.Wait(ctx)
	return jsonRoundTrip("OAuthCompletion", completion), roundTripError("OAuthFlow.Wait", err)
}

func (f codecOAuthFlow) Cancel() {
	f.inner.Cancel()
}

// EnterWorktree is class H: a non-nil error round-trips, and the
// returned Workspace is wrapped in NewLoopback so every call made
// through it also goes through the codec; release passes through
// unchanged (it carries no data).
func (l *Loopback) EnterWorktree(ctx context.Context, name string) (workspace.Workspace, func(), error) {
	inner, release, err := l.inner.EnterWorktree(ctx, name)
	return wrapHandleWorkspace(inner), release, roundTripError("EnterWorktree", err)
}

// ExitWorktree is class H; see EnterWorktree.
func (l *Loopback) ExitWorktree(ctx context.Context) (workspace.Workspace, func(), error) {
	inner, release, err := l.inner.ExitWorktree(ctx)
	return wrapHandleWorkspace(inner), release, roundTripError("ExitWorktree", err)
}

// AttachThread is class H; see EnterWorktree.
func (l *Loopback) AttachThread(ctx context.Context, id string) (workspace.Workspace, func(), error) {
	inner, release, err := l.inner.AttachThread(ctx, id)
	return wrapHandleWorkspace(inner), release, roundTripError("AttachThread", err)
}

// wrapHandleWorkspace wraps ws in a fresh Loopback so a Workspace handed
// back from an H method keeps every subsequent call on it running
// through the codec too, matching what a real wire hop would do (the
// caller only ever holds a client stub, never the server's own
// implementation). nil in, nil out: EnterWorktree/ExitWorktree/
// AttachThread all return a nil Workspace alongside a non-nil error.
func wrapHandleWorkspace(ws workspace.Workspace) workspace.Workspace {
	if ws == nil {
		return nil
	}
	return NewLoopback(ws)
}

// Shutdown is class X: client-side connection teardown once there is a
// real client, but a plain delegation here since Loopback has no
// connection of its own to tear down.
func (l *Loopback) Shutdown() {
	l.inner.Shutdown()
}

// PrepareSessionChanges used to need a hand-written forward here: it was
// an optional capability outside Workspace itself, resolved by a type
// assertion (root.go's old msg.ws.(workspace.SessionChangePreparer)) that
// could not survive Loopback -- a real wire defect the wire CI job
// (SENNIT_TEST_WIRE=1) caught. PR 0.7c's review folded it into
// FileServices as a guaranteed Workspace member instead (same fix applied
// to WorktreeState below), so it is class U now and the generator
// (zz_generated_loopback.go) covers it like any other U method.
