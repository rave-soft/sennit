package log

import "net/http"

// newHTTPClient returns a client whose transport is the logger under test.
//
// Production never builds a client this way: internal/agent and
// internal/oauth/copilot wrap whatever transport they already have, which
// is the only way to keep a provider's own client settings. A constructor
// in http.go that produced a fresh client around http.DefaultTransport
// therefore had no caller and quietly suggested the wrong thing.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &HTTPRoundTripLogger{
			Transport: http.DefaultTransport,
		},
	}
}
