package thread

// unwinder collects the undo for each step of a multi-step operation as
// that step succeeds, so a later step's failure can unwind exactly the
// resources already created — no more, no less — without a hand-written
// rollback call at every return site in between.
//
// Invariant: push must only be called once the step it undoes has
// actually succeeded. unwind then runs every pushed undo in the reverse
// order they were pushed (a spawned workspace is released before the
// worktree it was spawned into is removed), and never runs anything for a
// step that never got far enough to push one — nothing was ever pushed
// for it. commit disarms unwind for good once the operation has fully
// succeeded, so nothing pushed along the way is torn back down under it.
//
// Zero value is ready to use. Callers defer unwind() once, right after
// declaring the value, so every return path unwinds correctly with no
// rollback call of its own.
type unwinder struct {
	undo      []func()
	committed bool
}

// push registers undo as the action that reverses a step which has just
// succeeded. Order matters: undo runs in the reverse of push order.
func (u *unwinder) push(undo func()) {
	u.undo = append(u.undo, undo)
}

// commit disarms unwind: the operation reached a state worth keeping, so
// nothing pushed so far should be reverted.
func (u *unwinder) commit() {
	u.committed = true
}

// unwind runs every pushed undo, most recently pushed first, unless
// commit was already called.
func (u *unwinder) unwind() {
	if u.committed {
		return
	}
	for i := len(u.undo) - 1; i >= 0; i-- {
		u.undo[i]()
	}
}
