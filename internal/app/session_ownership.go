package app

import (
	"context"
	"errors"
	"sync"

	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/message"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
)

type liveSessionOwner struct {
	app     *App
	ownerID string
	epoch   int64
}

var sessionOwners sync.Map

func (app *App) ownershipIdentity() (string, int64, bool) {
	app.ownershipMu.RLock()
	defer app.ownershipMu.RUnlock()
	return app.ownerID, app.ownerEpoch, app.ownershipRequired
}

func (app *App) checkSessionOwnership(ctx context.Context, sessionID string) error {
	ownerID, epoch, required := app.ownershipIdentity()
	if !required {
		return nil
	}
	if app.ownership == nil || epoch == 0 {
		return ErrSessionOwnershipLost
	}
	owned, err := app.ownership.IsOwner(ctx, sessionID, ownerID, epoch)
	if err != nil {
		return err
	}
	if !owned {
		app.unregisterSessionOwner(sessionID, ownerID, epoch)
		return ErrSessionOwnershipLost
	}
	return nil
}

// ClaimSessionOwnership creates initial ownership or verifies that this App
// already owns it. It never adopts an existing owner; restart recovery is an
// explicit operation so a second live App at the same root cannot steal.
func (app *App) ClaimSessionOwnership(ctx context.Context, sessionID string) (sessionstore.Ownership, error) {
	if app.ownership == nil {
		return sessionstore.Ownership{}, errors.New("session ownership store is unavailable")
	}
	ownerID, epoch, required := app.ownershipIdentity()
	root := fsext.Canonical(app.config.WorkingDir())
	o, err := app.ownership.Claim(ctx, sessionID, ownerID, root)
	if err != nil {
		return sessionstore.Ownership{}, err
	}
	if required && o.OwnerID == ownerID && o.Epoch == epoch {
		app.registerSessionOwner(sessionID, o)
		return o, nil
	}
	if o.OwnerID != ownerID || o.OwnerRoot != root {
		return sessionstore.Ownership{}, ErrSessionOwnershipLost
	}
	app.AdoptSessionOwnership(sessionID, ownerID, o.Epoch)
	return o, nil
}

// RecoverSessionOwnership explicitly adopts a dead runtime at the durable
// source location. Process-local liveness prevents adoption while that exact
// persisted owner is still registered.
func (app *App) RecoverSessionOwnership(ctx context.Context, sessionID string) (sessionstore.Ownership, error) {
	if app.ownership == nil {
		return sessionstore.Ownership{}, errors.New("session ownership store is unavailable")
	}
	o, err := app.ownership.Get(ctx, sessionID)
	if err != nil {
		return sessionstore.Ownership{}, err
	}
	if live, ok := sessionOwners.Load(sessionID); ok {
		entry := live.(liveSessionOwner)
		if entry.ownerID == o.OwnerID && entry.epoch == o.Epoch && entry.app != app {
			return sessionstore.Ownership{}, ErrSessionOwnershipLive
		}
	}
	ownerID, _, _ := app.ownershipIdentity()
	o, err = app.ownership.Adopt(ctx, sessionID, ownerID, fsext.Canonical(app.config.WorkingDir()))
	if err != nil {
		return sessionstore.Ownership{}, err
	}
	app.AdoptSessionOwnership(sessionID, ownerID, o.Epoch)
	return o, nil
}

func (app *App) ArmPreparedSession() {
	app.ownershipMu.Lock()
	app.ownershipRequired = true
	app.ownerEpoch = 0
	app.ownershipMu.Unlock()
}

func (app *App) AdoptSessionOwnership(sessionID, ownerID string, epoch int64) {
	app.ownershipMu.Lock()
	app.ownerID = ownerID
	app.ownerEpoch = epoch
	app.ownershipRequired = true
	app.ownershipMu.Unlock()
	app.registerSessionOwner(sessionID, sessionstore.Ownership{OwnerID: ownerID, Epoch: epoch})
}

func (app *App) registerSessionOwner(sessionID string, ownership sessionstore.Ownership) {
	sessionOwners.Store(sessionID, liveSessionOwner{app: app, ownerID: ownership.OwnerID, epoch: ownership.Epoch})
}

func (app *App) unregisterSessionOwner(sessionID, ownerID string, epoch int64) {
	entry := liveSessionOwner{app: app, ownerID: ownerID, epoch: epoch}
	sessionOwners.CompareAndDelete(sessionID, entry)
}

func (app *App) UnregisterSessionOwnership() {
	sessionID := app.CurrentSessionID()
	ownerID, epoch, required := app.ownershipIdentity()
	if sessionID != "" && required {
		app.unregisterSessionOwner(sessionID, ownerID, epoch)
	}
}

// ResolveCurrentSessionOwner maps durable identity to a registered live App.
// Missing or stale registrations resolve to nil so the outbox remains pending.
func (app *App) ResolveCurrentSessionOwner(ctx context.Context, sessionID string) *App {
	if app.ownership == nil {
		return nil
	}
	o, err := app.ownership.Get(ctx, sessionID)
	if err != nil {
		return nil
	}
	value, ok := sessionOwners.Load(sessionID)
	if !ok {
		return nil
	}
	entry := value.(liveSessionOwner)
	if entry.ownerID != o.OwnerID || entry.epoch != o.Epoch {
		sessionOwners.CompareAndDelete(sessionID, entry)
		return nil
	}
	return entry.app
}

func (app *App) OwnerIdentity() (string, int64) {
	ownerID, epoch, _ := app.ownershipIdentity()
	return ownerID, epoch
}

func (app *App) OwnershipStore() *sessionstore.OwnershipStore { return app.ownership }

func (app *App) SetOwnershipForTest(store *sessionstore.OwnershipStore, ownerID string) {
	app.ownershipMu.Lock()
	app.ownership = store
	app.ownerID = ownerID
	app.ownershipMu.Unlock()
	app.agentDispatcher.SetOwnershipCheck(app.checkSessionOwnership)
}

func (app *App) SessionQuiescent(ctx context.Context, sessionID string) (bool, error) {
	coordinator := app.Coordinator()
	if coordinator != nil && coordinator.IsSessionBusy(sessionID) {
		return false, nil
	}
	messages, err := app.Messages().List(ctx, sessionID)
	if err != nil {
		return false, err
	}
	for _, msg := range messages {
		results := make(map[string]struct{}, len(msg.ToolResults()))
		for _, result := range msg.ToolResults() {
			results[result.ToolCallID] = struct{}{}
		}
		for _, call := range msg.ToolCalls() {
			if !call.Finished {
				return false, nil
			}
			if _, ok := results[call.ID]; !ok && !call.ProviderExecuted {
				return false, nil
			}
		}
		if msg.Role == message.Assistant && !msg.IsFinished() {
			return false, nil
		}
	}
	return true, nil
}

func (app *App) WithOwnershipFence(ctx context.Context, sessionID string, apply func() error) error {
	return app.agentDispatcher.WithAdmissionClosed(func() error {
		if err := app.checkSessionOwnership(ctx, sessionID); err != nil {
			return err
		}
		return apply()
	})
}

// WithSessionTransferGate serializes preflight or commit with dispatch
// admission, verifies durable ownership, and rejects active turns/tools.
func (app *App) WithSessionTransferGate(ctx context.Context, sessionID string, transfer func() error) error {
	return app.agentDispatcher.WithAdmissionClosed(func() error {
		if err := app.checkSessionOwnership(ctx, sessionID); err != nil {
			return err
		}
		quiescent, err := app.SessionQuiescent(ctx, sessionID)
		if err != nil {
			return err
		}
		if !quiescent {
			return ErrSessionBusy
		}
		return transfer()
	})
}

var (
	ErrSessionBusy          = errors.New("session is busy")
	ErrSessionOwnershipLive = errors.New("session ownership is held by a live runtime")
)

func (app *App) CurrentSessionID() string {
	live := app.liveSession.Load()
	if live == nil {
		return ""
	}
	return *live
}
