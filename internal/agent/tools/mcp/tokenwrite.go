package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
)

// currentGen returns a server's current generation without bumping it.
func (r *Registry) currentGen(name string) uint64 {
	g, _ := r.gens.Get(name)
	return g
}

func (r *Registry) beginAttempt(name string) (attemptID, error) {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if r.closing {
		return attemptID{}, errors.New("mcp registry is closing")
	}
	owner := attemptID{gen: r.currentGen(name), seq: r.authAttempt.Add(1)}
	r.owners[name] = owner
	return owner, nil
}

func (r *Registry) reserveTokenMutation(cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID) bool {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !r.ownsLocked(name, owner) {
		return false
	}
	key := tokenWriteOwner{name: name, attempt: owner}
	if _, ok := r.tokenReservations[key]; ok {
		return true
	}
	reservation, ok := cfg.ReserveMCPTokenMutation(name, m)
	if ok {
		r.tokenReservations[key] = &reservation
	}
	return ok
}

func (r *Registry) owns(name string, owner attemptID) bool {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	return r.ownsLocked(name, owner)
}

func (r *Registry) ownsLocked(name string, owner attemptID) bool {
	return !r.closing && owner.valid() && r.currentGen(name) == owner.gen && r.owners[name] == owner
}

func (r *Registry) beginTokenWrite(name string, owner attemptID) *tokenWrite {
	if r.beforeTokenPersist != nil {
		r.beforeTokenPersist()
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	state, hasState := r.states.Get(name)
	key := tokenWriteOwner{name: name, attempt: owner}
	if !r.ownsLocked(name, owner) || (hasState && state.State == StateDisabled) {
		return nil
	}
	if _, ok := r.tokenReservations[key]; !ok {
		return nil
	}
	write := &tokenWrite{done: make(chan struct{})}
	if r.tokenWrites[key] == nil {
		r.tokenWrites[key] = map[*tokenWrite]struct{}{}
	}
	r.tokenWrites[key][write] = struct{}{}
	return write
}

func (r *Registry) finishTokenWrite(name string, owner attemptID, write *tokenWrite) {
	r.publishMu.Lock()
	key := tokenWriteOwner{name: name, attempt: owner}
	delete(r.tokenWrites[key], write)
	if len(r.tokenWrites[key]) == 0 {
		delete(r.tokenWrites, key)
	}
	close(write.done)
	r.publishMu.Unlock()
}

func (r *Registry) persistOAuthToken(ctx context.Context, cfg ConfigProvider, name string, owner attemptID, tok *oauth.Token) {
	if m, ok := cfg.Config().MCP[name]; ok && !r.reserveTokenMutation(cfg, name, m, owner) {
		return
	}
	write := r.beginTokenWrite(name, owner)
	if write == nil {
		return
	}
	defer r.finishTokenWrite(name, owner, write)
	key := fmt.Sprintf("mcp.%s.oauth_token", name)
	if err := r.tokenPersist(ctx, cfg, key, tok); err != nil {
		slog.Warn("Failed to persist MCP OAuth token", "name", name, "error", err)
		return
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !r.ownsLocked(name, owner) {
		return
	}
	reservation, ok := r.tokenReservations[tokenWriteOwner{name: name, attempt: owner}]
	if !ok {
		return
	}
	if err := r.tokenCommit(cfg, reservation, tok); err != nil {
		slog.Warn("Failed to persist MCP OAuth token", "name", name, "error", err)
	}
}

func (r *Registry) tokenWriteWaitersLocked(name string) []<-chan struct{} {
	var waiters []<-chan struct{}
	for owner, writes := range r.tokenWrites {
		if name != "" && owner.name != name {
			continue
		}
		for write := range writes {
			waiters = append(waiters, write.done)
		}
	}
	return waiters
}

func waitTokenWrites(ctx context.Context, waiters []<-chan struct{}) error {
	for _, done := range waiters {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
