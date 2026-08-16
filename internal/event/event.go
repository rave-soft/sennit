// Package event used to report usage analytics to Charm's PostHog instance at
// data.charm.land. That reporting is removed: Sennit sends nothing anywhere.
//
// The call sites are kept as no-ops rather than deleted so that adding a
// self-hosted sink later is a change in one file instead of a hunt through
// nine.
package event

// send is where an event would be recorded. It does nothing.
func send(string, ...any) {}

// SetNonInteractive records nothing.
func SetNonInteractive(bool) {}

// SetContinueBySessionID records nothing.
func SetContinueBySessionID(bool) {}

// SetContinueLastSession records nothing.
func SetContinueLastSession(bool) {}

// Init previously created the analytics client and a persistent machine
// identifier. Both are gone; nothing is initialised.
func Init() {}

// GetID returns an empty string. There is no machine identifier to report.
func GetID() string { return "" }

// Alias records nothing.
func Alias(string) {}

// Error records nothing. Errors still reach the local log through slog.
func Error(any, ...any) {}

// Flush has nothing to flush.
func Flush() {}
