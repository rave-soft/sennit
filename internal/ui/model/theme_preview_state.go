package model

// themePreviewState owns the palette currently applied to the UI and the
// restore point for an in-progress theme preview. It intentionally has no
// dependency on styles, configuration, commands, or UI components: UI
// validates palette IDs, applies/restyles a palette, and persists a confirmed
// selection around this state machine.
type themePreviewState struct {
	liveID      string
	previewFrom string
}

// live returns the applied palette, or configured when no palette has been
// explicitly applied during this UI's lifetime.
func (s *themePreviewState) live(configured string) string {
	if s.liveID != "" {
		return s.liveID
	}
	return configured
}

// setLive records the palette UI has successfully applied to its components.
func (s *themePreviewState) setLive(id string) {
	s.liveID = id
}

// preview starts a preview if needed and reports whether id differs from the
// currently applied palette. The first browse records the restore point;
// subsequent browses preserve it.
func (s *themePreviewState) preview(id, configured string) bool {
	live := s.live(configured)
	if s.previewFrom == "" {
		s.previewFrom = live
	}
	return id != live
}

// confirm consumes an in-progress preview without restoring its origin.
func (s *themePreviewState) confirm() {
	s.previewFrom = ""
}

// previewActive reports whether a preview still has a restore point.
func (s *themePreviewState) previewActive() bool {
	return s.previewFrom != ""
}

// cancel consumes an in-progress preview and reports the palette UI must
// restore. It only requests restoration when the current palette differs from
// the original one.
func (s *themePreviewState) cancel(live string) (restore string, needed bool) {
	from := s.previewFrom
	s.previewFrom = ""
	if from == "" || from == live {
		return "", false
	}
	return from, true
}
