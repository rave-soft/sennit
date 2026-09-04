package model

import (
	"sync"
	"testing"
)

// TestSetTheme_DoesNotRaceASnapshotOfTheOldStyles is the regression test
// for finding 5: setTheme used to overwrite *m.com.Styles's fields in
// place (`*m.com.Styles = ...`), which raced any command already holding
// that same pointer — beginSessionLoad snapshots `styles := m.com.Styles`
// on the Update goroutine and reads its fields later, off it, inside a
// tea.Cmd. Run with -race, the old in-place write and a concurrent read
// through a snapshot taken before the switch are a reported data race;
// setTheme now assigns m.com.Styles a fresh pointer instead, so the
// snapshot's own memory is never touched again.
func TestSetTheme_DoesNotRaceASnapshotOfTheOldStyles(t *testing.T) {
	m := newTestUI()

	// The exact pattern beginSessionLoad uses: read the pointer on what
	// stands in for the Update goroutine here, then hand it to what
	// stands in for the tea.Cmd goroutine.
	snapshot := m.com.Styles

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = snapshot.Rev()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			id := "light"
			if i%2 == 0 {
				id = "dark"
			}
			m.setTheme(id)
		}
	}()

	wg.Wait()
}
