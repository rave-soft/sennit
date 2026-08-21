package agent

// Test-only accessors onto dispatcher's per-session state. Production
// code never needs to poke a session's queue/active/cancel-mark fields
// directly from outside dispatch.go's own methods; these exist purely so
// whitebox tests can seed or inspect that state without reaching into
// sessionState's unexported fields at a dozen different call sites.

// setActiveForTest marks sessionID busy with ac, as if dispatchDecision
// had just registered it as the active run.
func (d *dispatcher) setActiveForTest(sessionID string, ac *activeCancel) {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	s.active = ac
	s.mu.Unlock()
}

// getActiveForTest returns sessionID's active-run handle, if any.
func (d *dispatcher) getActiveForTest(sessionID string) (*activeCancel, bool) {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.active != nil
}

// setMessageQueueForTest replaces sessionID's queued calls outright,
// bypassing enqueueCall's per-call bookkeeping (OnComplete stripping,
// acceptSeq stamping, queuedAt) so a test can seed exact queue contents.
func (d *dispatcher) setMessageQueueForTest(sessionID string, queue []SessionAgentCall) {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	s.messageQueue = queue
	s.mu.Unlock()
}

// getMessageQueueForTest returns sessionID's queued calls and whether
// anything is queued at all (mirroring the old map's Get semantics,
// which distinguished "no entry" from "entry present but empty").
func (d *dispatcher) getMessageQueueForTest(sessionID string) ([]SessionAgentCall, bool) {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messageQueue, len(s.messageQueue) > 0
}

// setCancelMarkForTest seeds sessionID's cancel high-water mark directly,
// as if a Cancel had already raised it, without going through cancel()
// itself.
func (d *dispatcher) setCancelMarkForTest(sessionID string, mark uint64) {
	s, release := d.session(sessionID)
	defer release()
	d.acceptedMu.Lock()
	s.cancelMark = mark
	d.acceptedMu.Unlock()
}

// acceptedRunsForTest returns sessionID's in-flight accepted-run count.
func (d *dispatcher) acceptedRunsForTest(sessionID string) int {
	s, release := d.session(sessionID)
	defer release()
	d.acceptedMu.Lock()
	defer d.acceptedMu.Unlock()
	return s.acceptedRuns
}

// cancelMarkForTest returns sessionID's cancel high-water mark.
func (d *dispatcher) cancelMarkForTest(sessionID string) uint64 {
	s, release := d.session(sessionID)
	defer release()
	d.acceptedMu.Lock()
	defer d.acceptedMu.Unlock()
	return s.cancelMark
}

// sessionCountForTest returns the number of sessions dispatcher
// currently holds state for, for tests asserting the refcounted
// leak fix actually reclaims idle sessions (see
// TestDispatcher_SessionStateReclaimedWhenIdle).
func (d *dispatcher) sessionCountForTest() int {
	return d.states.Len()
}

// sessionMuLocker is the minimal Lock/TryLock/Unlock surface a test needs
// to probe a session's dispatch mutex from outside dispatch.go without
// leaking the refcounting mechanics into the test itself.
type sessionMuLocker struct {
	d         *dispatcher
	sessionID string
	s         *sessionState
	release   func()
}

func (l *sessionMuLocker) Lock() { l.s.mu.Lock() }

func (l *sessionMuLocker) TryLock() bool { return l.s.mu.TryLock() }

// Unlock unlocks the underlying mutex and releases this locker's
// reference on the session state. Every acquire (sessionMuForTest) must
// be paired with exactly one Unlock, mirroring plain sync.Mutex usage.
func (l *sessionMuLocker) Unlock() {
	l.s.mu.Unlock()
	l.release()
}

// sessionMuForTest hands a test direct Lock/TryLock/Unlock access to
// sessionID's dispatch mutex, for structural checks like "this callback
// must never run while the mutex is still held" (see
// TestQueueChanged_NotPublishedUnderDispatchLock).
func (d *dispatcher) sessionMuForTest(sessionID string) *sessionMuLocker {
	s, release := d.session(sessionID)
	return &sessionMuLocker{d: d, sessionID: sessionID, s: s, release: release}
}
