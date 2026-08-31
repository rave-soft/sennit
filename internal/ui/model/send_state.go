package model

import "github.com/rave-soft/sennit/internal/message"

// pendingSendState owns work accepted by the editor but not yet handed to the
// active session. A generation identifies a pending new-session creation;
// active serializes sends once a session exists, and queue preserves their FIFO
// order until the matching session load completes.
type pendingSendState struct {
	queue      []sendQueueItem
	generation uint64
	loading    bool
	active     bool
}

// sendQueueItem is one message or bang command waiting for its target session.
type sendQueueItem struct {
	content        string
	attachments    []message.Attachment
	generation     uint64
	sessionID      string
	loadGeneration uint64
	bang           bool
	isFirstMessage bool
}

func (s *pendingSendState) enqueue(item sendQueueItem) {
	s.queue = append(s.queue, item)
}

func (s *pendingSendState) enqueueFront(item sendQueueItem) {
	s.queue = append([]sendQueueItem{item}, s.queue...)
}

func (s *pendingSendState) beginLoading() uint64 {
	s.loading = true
	s.generation++
	return s.generation
}

func (s *pendingSendState) acceptsLoadingResult(generation uint64) bool {
	return s.loading && s.matchesGeneration(generation)
}

func (s *pendingSendState) matchesGeneration(generation uint64) bool {
	return generation == s.generation
}

func (s *pendingSendState) loadingNow() bool {
	return s.loading
}

func (s *pendingSendState) generationNow() uint64 {
	return s.generation
}

func (s *pendingSendState) hasQueued() bool {
	return len(s.queue) > 0
}

func (s *pendingSendState) bindQueuedToSession(generation uint64, sessionID string, loadGeneration uint64) {
	for i := range s.queue {
		if s.queue[i].generation == generation {
			s.queue[i].sessionID = sessionID
			s.queue[i].loadGeneration = loadGeneration
		}
	}
}

func (s *pendingSendState) dequeue() (sendQueueItem, bool) {
	if s.active || len(s.queue) == 0 {
		return sendQueueItem{}, false
	}
	item := s.queue[0]
	s.queue = s.queue[1:]
	return item, true
}

func (s *pendingSendState) beginActive() {
	s.active = true
}

func (s *pendingSendState) activeNow() bool {
	return s.active
}

func (s *pendingSendState) finishActive() {
	s.active = false
}

func (s *pendingSendState) finishLoading() {
	s.loading = false
}

func (s *pendingSendState) discardForSessionChange() {
	s.queue = nil
	s.active = false
}

func (s *pendingSendState) discardForNewSession() {
	s.queue = nil
	s.generation = 0
	s.loading = false
}

func (s *pendingSendState) discardLoading() {
	s.queue = nil
	s.generation = 0
	s.loading = false
}

func (s *pendingSendState) rejectCreation() {
	s.queue = nil
	s.loading = false
	s.active = false
}
