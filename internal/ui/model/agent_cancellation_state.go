package model

// agentCancellationState owns the two-step confirmation lifecycle for
// cancelling an active agent turn.
type agentCancellationState struct {
	armed bool
}

// arm begins cancellation confirmation.
func (s *agentCancellationState) arm() {
	s.armed = true
}

// confirm consumes an armed confirmation and resets the lifecycle. It reports
// whether the caller should perform the cancellation.
func (s *agentCancellationState) confirm() bool {
	if !s.armed {
		return false
	}
	s.armed = false
	return true
}

// expire resets an armed confirmation after its UI-owned timer expires.
func (s *agentCancellationState) expire() {
	s.armed = false
}

func (s *agentCancellationState) isArmed() bool {
	return s.armed
}
