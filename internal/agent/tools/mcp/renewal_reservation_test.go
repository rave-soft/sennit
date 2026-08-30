package mcp

import (
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// TestBeginRenewal_DoesNotLeakTokenReservations pins the fix for
// tokenReservations growing by one entry per lazy renewal: a reservation
// is keyed by (server name, attempt), and beginRenewal used to hand the
// server a fresh attempt without ever removing the previous attempt's key
// - only teardown pruned the map, and a healthy OAuth server can renew
// many times over the life of the process without ever tearing down.
// Simulates several renewal rounds (each one reserving under its current
// owner, the way persistOAuthToken would, then getting superseded) and
// checks the map never accumulates more than the live owner's entry.
func TestBeginRenewal_DoesNotLeakTokenReservations(t *testing.T) {
	const name = "test-renewal-reservations"
	r := NewRegistry()
	session, _ := liveSession(t, "tool")

	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, session)
	r.sessionOwners[name] = owner
	r.publishMu.Unlock()

	for i := range 5 {
		r.publishMu.Lock()
		r.tokenReservations[tokenWriteOwner{name: name, attempt: owner}] = &config.MCPTokenMutation{}
		r.publishMu.Unlock()

		renewal, ok := r.beginRenewal(name, owner, session, errors.New("ping failed"))
		require.True(t, ok, "round %d: beginRenewal must succeed against the owner it was called with", i)

		r.publishMu.Lock()
		_, previousStillReserved := r.tokenReservations[tokenWriteOwner{name: name, attempt: owner}]
		count := len(r.tokenReservations)
		// Simulate the reconnect that would normally follow a renewal:
		// updateStateLocked(StateError) already took the session out of
		// r.sessions, so both it and sessionOwners must be republished
		// (see init.go) for the next round's beginRenewal call to find a
		// real current owner.
		r.sessions.Set(name, session)
		r.sessionOwners[name] = renewal
		r.publishMu.Unlock()

		require.False(t, previousStillReserved, "round %d: renewal must drop the previous owner's reservation", i)
		require.LessOrEqual(t, count, 1, "round %d: tokenReservations must not accumulate one entry per renewal", i)

		owner = renewal
	}
}

// TestBeginRenewal_DoesNotDropALiveDifferentOwnersReservation guards the
// other direction of the same fix: beginRenewal must only ever remove the
// exact (name, previous-owner) key it is retiring, never a reservation
// held by an unrelated server or a different, still-current owner.
func TestBeginRenewal_DoesNotDropALiveDifferentOwnersReservation(t *testing.T) {
	const name = "test-renewal-reservations-scope"
	const otherName = "test-renewal-reservations-other"
	r := NewRegistry()
	session, _ := liveSession(t, "tool")
	otherSession, _ := liveSession(t, "tool")

	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	otherOwner, err := r.beginAttempt(otherName)
	require.NoError(t, err)
	r.publishMu.Lock()
	r.sessions.Set(name, session)
	r.sessionOwners[name] = owner
	r.sessions.Set(otherName, otherSession)
	r.sessionOwners[otherName] = otherOwner
	r.tokenReservations[tokenWriteOwner{name: otherName, attempt: otherOwner}] = &config.MCPTokenMutation{}
	r.publishMu.Unlock()

	_, ok := r.beginRenewal(name, owner, session, errors.New("ping failed"))
	require.True(t, ok)

	r.publishMu.Lock()
	_, otherStillReserved := r.tokenReservations[tokenWriteOwner{name: otherName, attempt: otherOwner}]
	r.publishMu.Unlock()
	require.True(t, otherStillReserved, "a different server's live reservation must survive an unrelated renewal")
}
