package model

import (
	"time"

	"github.com/rave-soft/sennit/internal/ui/listcache"
)

// promptQueueState owns the memoized lifecycle of the active session's prompt
// queue. It deliberately knows neither how prompts are fetched nor which
// session is active; UI orchestrates workspace I/O, session identity, and
// layout around this state boundary.
type promptQueueState struct {
	cache listcache.TTLCache[[]string]
}

// prompts returns the last accepted queue, including while it is stale.
func (s *promptQueueState) prompts() []string {
	return s.cache.Value
}

func (s *promptQueueState) count() int {
	return len(s.cache.Value)
}

func (s *promptQueueState) inFlight() bool {
	return s.cache.InFlight
}

func (s *promptQueueState) fresh(ttl time.Duration) bool {
	return s.cache.Fresh(ttl)
}

// begin claims the sole in-flight fetch and returns its generation.
func (s *promptQueueState) begin() (uint64, bool) {
	return s.cache.Begin()
}

// complete finishes a fetch and reports whether no newer transition has
// superseded it. It must be called for every fetch result, including a result
// rejected by UI's session-scope check.
func (s *promptQueueState) complete(generation uint64) bool {
	return s.cache.Complete(generation)
}

// accept writes a current fetch result through as fresh.
func (s *promptQueueState) accept(prompts []string) {
	s.cache.Set(prompts)
}

// invalidate keeps the last accepted prompts visible but rejects any older
// in-flight result.
func (s *promptQueueState) invalidate() {
	s.cache.Invalidate()
}

// clear invalidates older work and writes the known empty queue through as
// fresh, for authoritative clear transitions such as Escape.
func (s *promptQueueState) clear() {
	s.invalidate()
	s.accept(nil)
}

// discard invalidates older work and removes prompts that belong to a departed
// session. Unlike clear, it remains stale so UI schedules a fetch for the new
// session.
func (s *promptQueueState) discard() {
	s.invalidate()
	s.cache.Value = nil
}
