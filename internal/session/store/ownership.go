package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/rave-soft/sennit/internal/session"
)

var (
	ErrNotOwner       = errors.New("session worktree: caller does not own session")
	ErrTransferActive = errors.New("session worktree: transfer already in progress")
	ErrInvalidPhase   = errors.New("session worktree: invalid transfer phase")
)

// Ownership is the durable single-writer fence for a session. Phase is
// stable or preparing; preparation never changes OwnerID.
type Ownership struct {
	SessionID    string
	OwnerID      string
	OwnerRoot    string
	WorktreeName string
	WorktreePath string
	Phase        string
	Epoch        int64
}

type OwnershipStore struct {
	db *sql.DB
	mu sync.Mutex
}

func NewOwnershipStore(db *sql.DB) *OwnershipStore { return &OwnershipStore{db: db} }

// Claim creates the initial main-root owner. Existing rows are returned
// unchanged, making bootstrap idempotent after restart.
func (s *OwnershipStore) Claim(ctx context.Context, sessionID, ownerID, root string) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO session_worktree_ownership (session_id, owner_id, owner_root) VALUES (?, ?, ?) ON CONFLICT(session_id) DO NOTHING`, sessionID, ownerID, root); err != nil {
		return Ownership{}, fmt.Errorf("claim session ownership: %w", err)
	}
	return s.get(ctx, sessionID)
}

// Adopt replaces the owner process only when it starts at the persisted
// stable location. Advancing the epoch fences a dead process after restart;
// a process at another root cannot steal the session.
func (s *OwnershipStore) Adopt(ctx context.Context, sessionID, ownerID, root string) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.get(ctx, sessionID)
	if err != nil {
		return Ownership{}, err
	}
	if o.Phase == "preparing" {
		if _, err := s.db.ExecContext(ctx, `UPDATE session_worktree_ownership SET phase = 'stable', target_owner_id = '', target_root = '', target_worktree_name = '', target_worktree_path = '', updated_at = strftime('%s','now') WHERE session_id = ? AND phase = 'preparing'`, sessionID); err != nil {
			return Ownership{}, fmt.Errorf("recover session transfer: %w", err)
		}
		o, err = s.get(ctx, sessionID)
		if err != nil {
			return Ownership{}, err
		}
	}
	if o.OwnerRoot != root {
		return Ownership{}, ErrNotOwner
	}
	if o.OwnerID == ownerID {
		return o, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE session_worktree_ownership SET owner_id = ?, epoch = epoch + 1, updated_at = strftime('%s','now') WHERE session_id = ? AND owner_id = ? AND epoch = ? AND phase = 'stable' AND owner_root = ?`, ownerID, sessionID, o.OwnerID, o.Epoch, root)
	if err != nil {
		return Ownership{}, fmt.Errorf("adopt session ownership: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Ownership{}, err
	}
	if n != 1 {
		return Ownership{}, ErrNotOwner
	}
	return s.get(ctx, sessionID)
}

func (s *OwnershipStore) Get(ctx context.Context, sessionID string) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(ctx, sessionID)
}

func (s *OwnershipStore) get(ctx context.Context, sessionID string) (Ownership, error) {
	var o Ownership
	err := s.db.QueryRowContext(ctx, `SELECT session_id, owner_id, owner_root, worktree_name, worktree_path, phase, epoch FROM session_worktree_ownership WHERE session_id = ?`, sessionID).Scan(&o.SessionID, &o.OwnerID, &o.OwnerRoot, &o.WorktreeName, &o.WorktreePath, &o.Phase, &o.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Ownership{}, fmt.Errorf("%w: %s", session.ErrNotFound, sessionID)
	}
	return o, err
}

// Prepare persists the target while source stays owner. Thus a process crash
// before Commit has one deterministic owner: the source.
func (s *OwnershipStore) Prepare(ctx context.Context, sessionID, ownerID string, epoch int64, target Ownership) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE session_worktree_ownership SET phase = 'preparing', target_owner_id = ?, target_root = ?, target_worktree_name = ?, target_worktree_path = ?, updated_at = strftime('%s','now') WHERE session_id = ? AND owner_id = ? AND epoch = ? AND phase = 'stable'`, target.OwnerID, target.OwnerRoot, target.WorktreeName, target.WorktreePath, sessionID, ownerID, epoch)
	if err != nil {
		return fmt.Errorf("prepare session transfer: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTransferActive
	}
	return nil
}

// Commit atomically installs the prepared target and advances the fencing
// epoch, so the former owner cannot accept another run afterwards.
func (s *OwnershipStore) Commit(ctx context.Context, sessionID, ownerID string, epoch int64) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE session_worktree_ownership SET owner_id = target_owner_id, owner_root = target_root, worktree_name = target_worktree_name, worktree_path = target_worktree_path, phase = 'stable', target_owner_id = '', target_root = '', target_worktree_name = '', target_worktree_path = '', epoch = epoch + 1, updated_at = strftime('%s','now') WHERE session_id = ? AND owner_id = ? AND epoch = ? AND phase = 'preparing' AND target_owner_id != ''`, sessionID, ownerID, epoch)
	if err != nil {
		return Ownership{}, fmt.Errorf("commit session transfer: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Ownership{}, err
	}
	if n != 1 {
		return Ownership{}, ErrInvalidPhase
	}
	return s.get(ctx, sessionID)
}

func (s *OwnershipStore) Rollback(ctx context.Context, sessionID, ownerID string, epoch int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE session_worktree_ownership SET phase = 'stable', target_owner_id = '', target_root = '', target_worktree_name = '', target_worktree_path = '', updated_at = strftime('%s','now') WHERE session_id = ? AND owner_id = ? AND epoch = ? AND phase = 'preparing'`, sessionID, ownerID, epoch)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrInvalidPhase
	}
	return nil
}

// Recover rolls an interrupted preparation back to the durable source owner.
// Worktree resources are retained because cleanup safety is unknown after a
// process crash.
func (s *OwnershipStore) Recover(ctx context.Context, sessionID string) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `UPDATE session_worktree_ownership SET phase = 'stable', target_owner_id = '', target_root = '', target_worktree_name = '', target_worktree_path = '', updated_at = strftime('%s','now') WHERE session_id = ? AND phase = 'preparing'`, sessionID); err != nil {
		return Ownership{}, fmt.Errorf("recover session transfer: %w", err)
	}
	return s.get(ctx, sessionID)
}

func (s *OwnershipStore) IsOwner(ctx context.Context, sessionID, ownerID string, epoch int64) (bool, error) {
	o, err := s.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return o.OwnerID == ownerID && o.Epoch == epoch, nil
}
