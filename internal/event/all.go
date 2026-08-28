package event

// sessionTelemetry adapts this package to sessionstore.TelemetrySink,
// the narrow seam internal/session/store reports its lifecycle through.
// The composition layer (internal/app and the session CLI commands)
// wires it into the service.
type sessionTelemetry struct{}

// NewSessionTelemetry returns the session lifecycle sink backed by this
// package's per-event no-ops.
func NewSessionTelemetry() sessionTelemetry {
	return sessionTelemetry{}
}

func (sessionTelemetry) SessionCreated() {
	SessionCreated()
}

func (sessionTelemetry) SessionDeleted() {
	SessionDeleted()
}

func AppInitialized() {
	send("app initialized")
}

func AppExited() {
	send("app exited")
	Flush()
}

func SessionCreated() {
	send("session created")
}

func SessionDeleted() {
	send("session deleted")
}

func FilePickerOpened() {
	send("filepicker opened")
}

func PromptSent(props ...any) {
	send(
		"prompt sent",
		props...,
	)
}

func PromptResponded(props ...any) {
	send(
		"prompt responded",
		props...,
	)
}

func TokensUsed(props ...any) {
	send(
		"tokens used",
		props...,
	)
}

func StatsViewed() {
	send("stats viewed")
}

func SessionListed(json bool) {
	send("session listed", "json", json)
}

func SessionShown(json bool) {
	send("session shown", "json", json)
}

func SessionLastShown(json bool) {
	send("session last shown", "json", json)
}

func SessionDeletedCommand(json bool) {
	send("session deleted", "json", json)
}

func SessionRenamed(json bool) {
	send("session renamed", "json", json)
}
