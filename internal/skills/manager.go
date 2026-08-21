package skills

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// Manager owns per-workspace skill discovery state: the latest discovery
// snapshot, the full skill metadata (with Instructions) for the
// coordinator, and a pubsub broker for change events. There is exactly
// one Manager per workspace.
//
// Package-level helpers (GetLatestStates, SetLatestStates,
// PublishStates, SubscribeEvents) are preserved for callers that share a
// process with the TUI. To bridge a Manager to those globals, construct
// it with WithGlobalMirror. Only do this for the top-level workspace;
// a spawned thread's workspace runs concurrently alongside it in the same
// process and must not enable mirroring (see
// app.BootstrapOptions.GlobalSkillsMirror).
type Manager struct {
	mu           sync.RWMutex
	allSkills    []*Skill
	activeSkills []*Skill
	states       []*SkillState

	// resolvedPaths are the expanded SkillsPaths used during discovery.
	// Stored so Catalog/ReadContent can label skills without
	// re-resolving.
	resolvedPaths []string
	workingDir    string

	// inherited are skills handed down by a parent workspace rather than
	// found on this workspace's own paths. Stored so a re-discovery
	// (WatchForChanges) can put them back instead of dropping them: they
	// are not on any path this workspace scans, so nothing else would
	// rediscover them.
	inherited []*Skill

	broker       *pubsub.Broker[Event]
	globalMirror bool
}

// ManagerOption configures a Manager at construction time.
type ManagerOption func(*Manager)

// WithGlobalMirror causes the manager to forward SetLatestStates and
// PublishStates calls to the package-level cache and broker. Only safe
// when the process hosts at most one Manager (i.e. the top-level
// workspace, not a spawned thread's workspace).
func WithGlobalMirror() ManagerOption {
	return func(m *Manager) {
		m.globalMirror = true
	}
}

// WithResolvedPaths stores the expanded skills directory paths that
// were used during discovery. Catalog and ReadContent use these for
// source labelling.
func WithResolvedPaths(paths []string) ManagerOption {
	return func(m *Manager) {
		m.resolvedPaths = paths
	}
}

// WithWorkingDir stores the workspace working directory. Catalog and
// ReadContent use it to distinguish project skills from user skills.
func WithWorkingDir(dir string) ManagerOption {
	return func(m *Manager) {
		m.workingDir = dir
	}
}

// WithInheritedSkills stores the skills a parent workspace handed to this
// one, so a later re-discovery can include them again. See
// DiscoveryConfig.InheritedSkills for why they exist.
func WithInheritedSkills(inherited []*Skill) ManagerOption {
	return func(m *Manager) {
		m.inherited = inherited
	}
}

// NewManager constructs a workspace-scoped Manager with the given
// pre-computed discovery results. The slices are stored as-is; callers
// should not mutate them afterwards.
func NewManager(allSkills, activeSkills []*Skill, states []*SkillState, opts ...ManagerOption) *Manager {
	m := &Manager{
		allSkills:    allSkills,
		activeSkills: activeSkills,
		states:       states,
		broker:       pubsub.NewBroker[Event](),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.globalMirror {
		SetLatestStates(states)
	}
	return m
}

// AllSkills returns the deduplicated list of all discovered skills.
func (m *Manager) AllSkills() []*Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.allSkills
}

// ActiveSkills returns the post-filter list of active skills (after
// removing disabled entries).
func (m *Manager) ActiveSkills() []*Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeSkills
}

// ResolvedPaths returns the expanded skills directory paths stored at
// construction time.
func (m *Manager) ResolvedPaths() []string {
	return m.resolvedPaths
}

// WorkingDir returns the workspace working directory stored at
// construction time.
func (m *Manager) WorkingDir() string {
	return m.workingDir
}

// InheritedSkills returns the skills handed down by a parent workspace.
// The watcher passes these back into DiscoverFromConfig so a re-discovery
// keeps them.
func (m *Manager) InheritedSkills() []*Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inherited
}

// ReplaceInherited swaps in a new set of parent-supplied skills. Called
// when the parent workspace re-discovers its own skills and pushes the
// result down, so a thread sees an edited SKILL.md without a restart.
func (m *Manager) ReplaceInherited(inherited []*Skill) {
	m.mu.Lock()
	m.inherited = inherited
	m.mu.Unlock()
}

// States returns a clone of the latest discovery state snapshot.
func (m *Manager) States() []*SkillState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneStates(m.states)
}

// SetLatestStates updates the manager's cached discovery snapshot.
func (m *Manager) SetLatestStates(states []*SkillState) {
	m.mu.Lock()
	m.states = cloneStates(states)
	m.mu.Unlock()
	if m.globalMirror {
		SetLatestStates(states)
	}
}

// PublishStates updates the manager's cached snapshot and publishes a
// discovery event to subscribers. Callers should not call
// SetLatestStates separately — PublishStates is the single mutation
// point, keeping Manager.States() (read by coordinator.skillStates for
// sennit_info's problems section) and (when WithGlobalMirror is set)
// skills.GetLatestStates consistent with what subscribers observe.
func (m *Manager) PublishStates(states []*SkillState) {
	m.mu.Lock()
	m.states = cloneStates(states)
	m.mu.Unlock()
	if m.globalMirror {
		SetLatestStates(states)
	}
	m.broker.Publish(pubsub.UpdatedEvent, Event{States: cloneStates(states)})
	if m.globalMirror {
		PublishStates(states)
	}
}

// ReplaceDiscovery atomically swaps in a freshly discovered skill set —
// used by the external-change watcher (see WatchForChanges) to hot-reload
// skills after a SKILL.md file is added, edited, or removed outside this
// process, without a restart. It updates AllSkills/ActiveSkills and
// publishes a discovery event via PublishStates, so subscribers see the
// same notification shape a normal discovery pass produces.
func (m *Manager) ReplaceDiscovery(allSkills, activeSkills []*Skill, states []*SkillState) {
	m.mu.Lock()
	m.allSkills = allSkills
	m.activeSkills = activeSkills
	m.mu.Unlock()
	m.PublishStates(states)
}

// SubscribeEvents returns a channel of discovery events for the
// manager's workspace.
func (m *Manager) SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return m.broker.Subscribe(ctx)
}

// Shutdown releases broker resources.
func (m *Manager) Shutdown() {
	if m.broker != nil {
		m.broker.Shutdown()
	}
}

// Inheritable returns the skills worth handing to a child workspace:
// everything except the builtins, which the child discovers from the same
// embedded FS and so would only duplicate.
//
// Each is copied rather than shared, and its location is rewritten to an
// InheritedPrefix address. A skill is loaded by reading the location the
// catalog advertises, and the original is an absolute path into the
// parent's checkout — somewhere a thread must not reach. The rewritten
// address is served from the skill's own Source instead, so the child
// needs nothing from the parent's filesystem. Copying also keeps the
// rewrite off the parent's own catalog, which still points at real files.
func Inheritable(all []*Skill) []*Skill {
	out := make([]*Skill, 0, len(all))
	for _, s := range all {
		if s == nil || s.Builtin {
			continue
		}
		clone := *s
		clone.Path = InheritedPrefix + s.Name
		clone.SkillFilePath = InheritedPrefix + s.Name + "/" + SkillFileName
		out = append(out, &clone)
	}
	return out
}

// DiscoverFromConfig walks the embedded builtin FS and every path in
// cfg.Options.SkillsPaths (after home / env expansion), then dedups and
// filters by cfg.Options.DisabledSkills. It returns the three slices the
// rest of the system needs:
//
//   - allSkills:    deduplicated, pre-filter (includes disabled).
//   - activeSkills: post-filter (DisabledSkills removed).
//   - states:       per-file discovery outcome for diagnostics/UI.
func DiscoverFromConfig(cfg DiscoveryConfig) (allSkills, activeSkills []*Skill, states []*SkillState) {
	builtin, builtinStates := DiscoverBuiltinWithStates()
	discovered := append([]*Skill(nil), builtin...)

	// Inherited skills sit between builtins and this workspace's own: a
	// parent's skill overrides a builtin of the same name, and the
	// workspace's own definition overrides both (Deduplicate keeps the
	// last occurrence).
	inheritedStates := make([]*SkillState, 0, len(cfg.InheritedSkills))
	for _, s := range cfg.InheritedSkills {
		if s == nil {
			continue
		}
		discovered = append(discovered, s)
		inheritedStates = append(inheritedStates, &SkillState{
			Name:  s.Name,
			Path:  s.SkillFilePath,
			State: StateNormal,
		})
	}

	var userStates []*SkillState
	userPaths := cfg.ResolvePaths()
	if len(userPaths) > 0 {
		var userSkills []*Skill
		userSkills, userStates = DiscoverWithStates(userPaths)
		discovered = append(discovered, userSkills...)
	}

	allSkills = Deduplicate(discovered)
	activeSkills = Filter(allSkills, cfg.DisabledSkills)

	allStates := append([]*SkillState(nil), builtinStates...)
	allStates = append(allStates, inheritedStates...)
	allStates = append(allStates, userStates...)
	allStates = DeduplicateStates(allStates)
	slices.SortStableFunc(allStates, func(a, b *SkillState) int {
		return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})
	return allSkills, activeSkills, allStates
}

// DiscoveryConfig contains the inputs DiscoverFromConfig needs. Using a
// dedicated struct (rather than importing internal/config) keeps the
// skills package's dependency graph small.
type DiscoveryConfig struct {
	SkillsPaths    []string
	DisabledSkills []string
	WorkingDir     string
	// InheritedSkills are skills a parent workspace already discovered
	// and handed down, for a child that cannot find them itself. A
	// thread runs in a git worktree, and a worktree has no
	// .sennit/skills of its own — the directory is not tracked, so it
	// does not come across with the checkout. Scanning the main
	// checkout instead would give a thread a filesystem reach outside
	// the worktree that isolates it, so the parent passes the parsed
	// skills down rather than the child reading back up. This
	// workspace's own DisabledSkills still apply to them.
	InheritedSkills []*Skill
	// Resolver expands $VAR-style references in paths. May be nil.
	Resolver func(string) (string, error)
}

// ResolvePaths expands home-directory and $VAR references in
// SkillsPaths. This is the canonical path-resolution logic used by
// DiscoverFromConfig; callers that need the resolved list (e.g. for
// Catalog labels) can call this directly.
func (c DiscoveryConfig) ResolvePaths() []string {
	if len(c.SkillsPaths) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.SkillsPaths))
	for _, pth := range c.SkillsPaths {
		expanded := home.Long(pth)
		if strings.HasPrefix(expanded, "$") && c.Resolver != nil {
			if resolved, err := c.Resolver(expanded); err == nil {
				expanded = resolved
			}
		}
		out = append(out, expanded)
	}
	return out
}
