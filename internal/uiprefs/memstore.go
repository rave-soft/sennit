package uiprefs

// MemStore is an in-memory [Store] for tests: a UI test harness that needs
// to drive a theme change, compact-mode toggle, or similar without a real
// [config.ConfigStore] behind it constructs one directly instead of faking
// out workspace.Config().
type MemStore struct {
	P Prefs
	// Sets records every Set call's key/value pair, in order, for tests
	// that want to assert what was written without re-deriving it from
	// P.
	Sets []MemStoreWrite
	// SetErr, when non-nil, is returned by Set and SetCompactMode instead
	// of applying the write, so a test can exercise the failure path.
	SetErr error
}

// MemStoreWrite records one [Store.Set] or [Store.SetCompactMode] call.
type MemStoreWrite struct {
	Key   string
	Value any
}

func (m *MemStore) Prefs() Prefs { return m.P }

func (m *MemStore) Set(key string, value any) error {
	if m.SetErr != nil {
		return m.SetErr
	}
	m.Sets = append(m.Sets, MemStoreWrite{Key: key, Value: value})
	applyMemStoreWrite(&m.P, key, value)
	return nil
}

func (m *MemStore) SetCompactMode(enabled bool) error {
	if m.SetErr != nil {
		return m.SetErr
	}
	m.Sets = append(m.Sets, MemStoreWrite{Key: "options.tui.compact_mode", Value: enabled})
	m.P.CompactMode = enabled
	return nil
}

// applyMemStoreWrite keeps MemStore.P consistent with the keys the real UI
// writes, so a test that toggles a pref through Set can immediately read it
// back from Prefs() without also updating P by hand.
func applyMemStoreWrite(p *Prefs, key string, value any) {
	switch key {
	case "options.tui.theme":
		if v, ok := value.(string); ok {
			p.ThemeID = v
		}
	case "options.tui.transparent":
		if v, ok := value.(bool); ok {
			p.TransparentEnabled = v
		}
	case "options.notifications":
		if v, ok := value.(string); ok {
			p.NotificationStyle = v
		}
	case "options.tui.compact_mode":
		if v, ok := value.(bool); ok {
			p.CompactMode = v
		}
	}
}
