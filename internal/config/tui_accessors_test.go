package config

import "testing"

// TestTUIAccessorsNilSafety verifies that every TUI-option accessor
// returns its documented zero value instead of panicking, whether the
// receiver is nil or a hand-built *Config with no Options at all (the
// shape every test — and internal/workspace's stubWorkspace — builds).
func TestTUIAccessorsNilSafety(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]*Config{
		"nil config":  nil,
		"bare config": {},
		"nil TUI":     {Options: &Options{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := cfg.TransparentEnabled(); got != false {
				t.Errorf("TransparentEnabled() = %v, want false", got)
			}
			depth, items := cfg.CompletionsLimits()
			if depth != 0 || items != 0 {
				t.Errorf("CompletionsLimits() = (%d, %d), want (0, 0)", depth, items)
			}
			if got := cfg.DiffMode(); got != "" {
				t.Errorf("DiffMode() = %q, want \"\"", got)
			}
			if got := cfg.Scrollbar(); got != "" {
				t.Errorf("Scrollbar() = %q, want \"\"", got)
			}
			if got := cfg.Keybindings(); got != nil {
				t.Errorf("Keybindings() = %v, want nil", got)
			}
			if got := cfg.CompactMode(); got != false {
				t.Errorf("CompactMode() = %v, want false", got)
			}
		})
	}
}
