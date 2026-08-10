package skills

import (
	"sort"
	"sync"
)

// Tracker tracks which skills have been loaded (read) during a session.
// It is safe for concurrent use.
//
// Note: Tracking is name-based and limited to active skills only. If a builtin
// skill is overridden by a user skill, only the user skill (which is active)
// can be marked as loaded. This prevents misattribution when reading builtin
// files that have been overridden.
type Tracker struct {
	mu          sync.RWMutex
	loaded      map[string]bool
	activeNames map[string]bool // Set of active skill names (post-dedup, post-filter)
}

// NewTracker creates a new skill tracker with the given active skill names.
// Only skills in activeSkills can be marked as loaded.
func NewTracker(activeSkills []*Skill) *Tracker {
	activeNames := make(map[string]bool, len(activeSkills))
	for _, s := range activeSkills {
		activeNames[s.Name] = true
	}
	return &Tracker{
		loaded:      make(map[string]bool),
		activeNames: activeNames,
	}
}

// MarkLoaded marks a skill as having been loaded.
// Only marks as loaded if the skill is in the active set (not overridden/disabled).
func (t *Tracker) MarkLoaded(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Only track if this skill is actually active (not overridden by user skill).
	if t.activeNames[name] {
		t.loaded[name] = true
	}
}

// IsLoaded returns true if the skill has been loaded.
func (t *Tracker) IsLoaded(name string) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.loaded[name]
}

// UpdateActiveSkills replaces the set of trackable skill names after a
// rescan (see Manager.ReplaceDiscovery), pruning loaded entries for
// skills that are no longer active. It deliberately does not clear
// loaded state wholesale: a skill that stays active across the rescan
// must stay "loaded" if it was read earlier this session, or the
// braid_info tool would misreport it as never having been read.
func (t *Tracker) UpdateActiveSkills(activeSkills []*Skill) {
	if t == nil {
		return
	}
	activeNames := make(map[string]bool, len(activeSkills))
	for _, s := range activeSkills {
		activeNames[s.Name] = true
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.activeNames = activeNames
	for name := range t.loaded {
		if !activeNames[name] {
			delete(t.loaded, name)
		}
	}
}

// LoadedNames returns the names of all skills that have been loaded, sorted
// alphabetically. Safe to call on a nil Tracker (returns nil).
func (t *Tracker) LoadedNames() []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.loaded) == 0 {
		return nil
	}
	names := make([]string, 0, len(t.loaded))
	for name := range t.loaded {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LoadedCount returns the number of unique skills that have been loaded.
// Safe to call on a nil Tracker (returns 0).
func (t *Tracker) LoadedCount() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.loaded)
}
