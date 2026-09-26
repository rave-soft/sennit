package wsrpc

import (
	"context"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
)

// The methods below are Loopback's S/H/X coverage: the generator
// (zz_generated_loopback.go) only emits U/C methods, so these are
// hand-written, plain delegation to inner with no codec involved. The
// codec for S (streamed) and H (handle-returning) methods comes in a later
// PR (CLIENT-SERVER.md, Фаза 1, PR 1.2/1.3); Shutdown (X) never crosses the
// wire at all. This file exists so *Loopback satisfies workspace.Workspace
// in full — see the compile-time assertion below.
var _ workspace.Workspace = (*Loopback)(nil)

// AgentRunShellCommand is class S.
func (l *Loopback) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	return l.inner.AgentRunShellCommand(ctx, sessionID, command, termWidth, onProgress, isFirstMessage)
}

// AgentRunStream is class S.
func (l *Loopback) AgentRunStream(ctx context.Context, sessionID, prompt string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	return l.inner.AgentRunStream(ctx, sessionID, prompt, opts)
}

// Subscribe is class S.
func (l *Loopback) Subscribe(send func(any)) {
	l.inner.Subscribe(send)
}

// SubscribeWith is class S.
func (l *Loopback) SubscribeWith(send func(any)) func() {
	return l.inner.SubscribeWith(send)
}

// StartOAuth is class H.
func (l *Loopback) StartOAuth(ctx context.Context, providerID, proxyURL string, forceNewAccount bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	return l.inner.StartOAuth(ctx, providerID, proxyURL, forceNewAccount)
}

// EnterWorktree is class H.
func (l *Loopback) EnterWorktree(ctx context.Context, name string) (workspace.Workspace, func(), error) {
	return l.inner.EnterWorktree(ctx, name)
}

// ExitWorktree is class H.
func (l *Loopback) ExitWorktree(ctx context.Context) (workspace.Workspace, func(), error) {
	return l.inner.ExitWorktree(ctx)
}

// AttachThread is class H.
func (l *Loopback) AttachThread(ctx context.Context, id string) (workspace.Workspace, func(), error) {
	return l.inner.AttachThread(ctx, id)
}

// Shutdown is class X: client-side connection teardown once there is a
// real client, but a plain delegation here since Loopback has no
// connection of its own to tear down.
func (l *Loopback) Shutdown() {
	l.inner.Shutdown()
}
