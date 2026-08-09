package config

import "sync"

// resetProviderState clears the memoized provider list so each test observes a
// fresh load. Providers() computes once per process via providerOnce; without
// this reset the first test to call it would fix the result for the rest.
//
// Upstream also reset the catalog syncers here. Braid has none: the provider
// catalog is the embedded one and never fetched.
func resetProviderState() {
	providerOnce = sync.Once{}
	providerList = nil
	providerErr = nil
}
