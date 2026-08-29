package agent

import "context"

// The three below are reachable only from tests, and their production
// files described audiences that do not exist: enqueueCall's doc offered
// itself to "anywhere that doesn't already hold the lock", the cache's
// invalidate offered an invalidation that does not publish. Production
// takes neither. Keeping them here leaves the production files describing
// what production does.

// enqueueCall appends call to the session's message queue, taking the
// session's dispatch mutex itself. dispatchDecision's busy branch calls
// enqueueLocked instead, already holding it. Like enqueueLocked, this does
// not notify: the caller does that once it has dropped the lock.
func (d *dispatcher) enqueueCall(call SessionAgentCall) {
	s, release := d.session(call.SessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	enqueueLocked(s, call)
}

// invalidate drops the cache without publishing a generation. Every
// production invalidation publishes — see invalidateAndPublish, whose doc
// explains why the two steps have to happen under one lock — so this shape
// exists only to drive the cache from a test.
func (c *runtimeCache) invalidate(ctx context.Context, reason string, key runtimeKey) {
	c.invalidateAndPublish(ctx, reason, key, nil)
}

// stats reads the cache counters. Nothing in production reports them; the
// counters themselves are live, and reach the log through runtimeCache.log.
func (c *runtimeCache) stats() runtimeCacheStats {
	return runtimeCacheStats{Hits: c.hits.Load(), Misses: c.misses.Load(), Builds: c.builds.Load(), Invalidations: c.invalidations.Load()}
}
