package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// Opening /stats before any session exists must not panic: m.sess.current
// is a pointer that stays nil until a session is loaded, and the screen
// handles an empty session id by saying its Session tab has nothing to
// report. Dereferencing it unguarded crashed the whole TUI.
func TestOpenStatsDialog_WithoutASession(t *testing.T) {
	t.Parallel()

	m := newTestUI()
	m.dialog = dialog.NewOverlay()
	require.Nil(t, m.sess.current, "this test is only meaningful with no session loaded")

	require.NotPanics(t, func() { m.openStatsDialog() })
	require.True(t, m.dialog.ContainsDialog(dialog.StatsID))
}

// With a session loaded, the dialog gets its id — that is what the
// Session tab reports on.
func TestOpenStatsDialog_WithASession(t *testing.T) {
	t.Parallel()

	m := newTestUI()
	m.dialog = dialog.NewOverlay()
	m.sess.current = &session.Session{ID: "sess-42"}

	require.NotPanics(t, func() { m.openStatsDialog() })
	require.True(t, m.dialog.ContainsDialog(dialog.StatsID))
}
