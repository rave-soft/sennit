// Package uiprefs isolates the handful of config fields that control how
// the TUI presents itself (theme, compact mode, keybindings, and the like)
// from the rest of the workspace-facing config. In-process today, these
// fields still live in the same sennitrc/sennit.json the agent reads; once
// a remote daemon exists, they will come from the client's own local
// config instead of the server's merged one. Isolating them behind [Store]
// now means the UI never reads them through [workspace.Workspace] and so
// does not have to change when that split lands.
package uiprefs

import "github.com/rave-soft/sennit/internal/config"

// Prefs is the read side: a snapshot of every config field the TUI uses to
// decide how to present itself, already resolved to the value a caller can
// use directly (fallbacks applied, no nil Options/TUI to check).
type Prefs struct {
	// ThemeID is the configured palette ID, or empty when unset. Both the
	// empty string and an unrecognized ID resolve to the default palette
	// (see styles.PaletteByID); this package does not know palette IDs.
	ThemeID string
	// SpinnerMode is the working-indicator motion, already defaulted to
	// "scramble" for an unset or unrecognized value (config.SpinnerScramble
	// and friends).
	SpinnerMode string
	// Scrollbar is the configured chat scrollbar visibility, or empty when
	// unset (callers fall back to config.ScrollbarDefault themselves).
	Scrollbar string
	// Keybindings holds per-action key overrides, or nil when none are
	// configured.
	Keybindings map[string][]string
	// CompactMode reports whether the TUI should start in compact mode.
	CompactMode bool
	// DiffMode is the configured TUI diff mode ("unified"/"split"), or
	// empty when unset.
	DiffMode string
	// TransparentEnabled reports whether the TUI's transparent background
	// is turned on.
	TransparentEnabled bool
	// CompletionsDepth and CompletionsItems are the @-mention completions
	// popup's directory-depth and item-count limits; 0 means unlimited.
	CompletionsDepth int
	CompletionsItems int
	// ProgressEnabled reports whether the indeterminate progress bar
	// should show during long operations. Defaults to true.
	ProgressEnabled bool
	// NotificationStyle is the configured notification backend
	// ("auto", "native", "osc", "bell", "disabled"). Defaults to "auto".
	NotificationStyle string
}

// Store is how the UI reads and writes its own display preferences. It
// deliberately does not expose a [config.Scope]: every write here lands in
// the global config, the only layer a client will still control once
// project-scoped config lives on a remote server (see CLIENT-SERVER.md,
// "PR 0.5b").
type Store interface {
	// Prefs returns the current preferences. Implementations do not cache:
	// each call reflects the latest reload, just as reading through
	// [workspace.Workspace.Config] does today.
	Prefs() Prefs
	// Set writes a single config key at global scope, keyed the same way
	// [workspace.ConfigFieldEditor.SetConfigField] is (e.g.
	// "options.tui.theme").
	Set(key string, value any) error
	// SetCompactMode sets compact mode at global scope. It is a typed
	// setter (rather than Set("options.tui.compact_mode", ...)) so the
	// in-process adapter can keep using [config.ConfigStore.SetCompactMode],
	// which skips the full disk reparse that a generic field write incurs.
	SetCompactMode(enabled bool) error
}

// FromConfig resolves a [Prefs] snapshot from a merged [config.Config], the
// same fallbacks the UI applied when it read these fields from Config()
// directly.
func FromConfig(cfg *config.Config) Prefs {
	spinnerMode, _ := cfg.SpinnerMode()
	depth, items := cfg.CompletionsLimits()

	progress := true
	notifications := "auto"
	if cfg != nil && cfg.Options != nil {
		if cfg.Options.Progress != nil {
			progress = *cfg.Options.Progress
		}
		if cfg.Options.Notifications != "" {
			notifications = cfg.Options.Notifications
		}
	}

	return Prefs{
		ThemeID:            cfg.ThemeID(),
		SpinnerMode:        spinnerMode,
		Scrollbar:          cfg.Scrollbar(),
		Keybindings:        cfg.Keybindings(),
		CompactMode:        cfg.CompactMode(),
		DiffMode:           cfg.DiffMode(),
		TransparentEnabled: cfg.TransparentEnabled(),
		CompletionsDepth:   depth,
		CompletionsItems:   items,
		ProgressEnabled:    progress,
		NotificationStyle:  notifications,
	}
}

// configStoreAdapter is the in-process [Store] implementation; see
// [NewConfigStoreAdapter].
type configStoreAdapter struct {
	store *config.ConfigStore
}

// NewConfigStoreAdapter wraps an application's [config.ConfigStore] as a
// [Store]. This is the in-process implementation: reads always resolve
// from the store's current published Config snapshot (no caching, so a
// reload is visible immediately), and writes go through the same
// ConfigStore calls the UI used to make directly.
func NewConfigStoreAdapter(store *config.ConfigStore) Store {
	return &configStoreAdapter{store: store}
}

func (a *configStoreAdapter) Prefs() Prefs {
	return FromConfig(a.store.Config())
}

func (a *configStoreAdapter) Set(key string, value any) error {
	return a.store.SetConfigField(config.ScopeGlobal, key, value)
}

func (a *configStoreAdapter) SetCompactMode(enabled bool) error {
	return a.store.SetCompactMode(config.ScopeGlobal, enabled)
}
