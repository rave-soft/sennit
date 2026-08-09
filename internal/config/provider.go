package config

import (
	"sync"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
)

var (
	providerOnce sync.Once
	providerList []catwalk.Provider
	providerErr  error
)

// Providers returns the list of providers, taking into account cached results
// and whether or not auto update is enabled.
//
// It will:
// 1. if auto update is disabled, it'll return the embedded providers at the
// time of release.
// 2. load the cached providers
// 3. try to get the fresh list of providers, and return either this new list,
// the cached list, or the embedded list if all others fail.
//
// A returned error is advisory: it reports that the catalog could not be
// cached, or that an upstream returned nothing usable. It never means that no
// providers are available, so callers should surface it as a warning and keep
// using the returned list. A refresh that simply could not reach the network
// is not an error at all: the cached or embedded catalog is a sound answer, so
// those are logged and the fallback is returned.
func Providers(cfg *Config) ([]catwalk.Provider, error) {
	providerOnce.Do(func() {
		// The provider catalog ships with the binary. Upstream refreshed it
		// from Charm's catwalk service at startup; that call is removed, so
		// Braid never reaches the network to decide what models exist.
		// Anything the embedded list does not know about is declared in the
		// user's own config.
		if !cfg.Options.DisableDefaultProviders {
			providerList = embedded.GetAll()
		}
	})
	return providerList, providerErr
}
