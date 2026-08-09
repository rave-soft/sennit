package config

import (
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
)

// Providers returns the provider catalog for cfg.
//
// It used to memoize the result process-globally via sync.Once, but that
// cached the first cfg it ever saw and kept returning that answer to every
// other *Config passed in afterwards — fatal once a single process can hold
// several ConfigStores (e.g. one per workspace) with different
// DisableDefaultProviders settings. embedded.GetAll() is just an in-memory
// slice, so recomputing per call is cheap; callers that already hold a
// ConfigStore should prefer its cached ConfigStore.KnownProviders() instead
// of calling this repeatedly.
//
// A returned error is advisory: it reports that the catalog could not be
// cached, or that an upstream returned nothing usable. It never means that no
// providers are available, so callers should surface it as a warning and keep
// using the returned list. A refresh that simply could not reach the network
// is not an error at all: the cached or embedded catalog is a sound answer, so
// those are logged and the fallback is returned.
func Providers(cfg *Config) ([]catwalk.Provider, error) {
	// The provider catalog ships with the binary. Upstream refreshed it
	// from Charm's catwalk service at startup; that call is removed, so
	// Braid never reaches the network to decide what models exist.
	// Anything the embedded list does not know about is declared in the
	// user's own config.
	if cfg.Options.DisableDefaultProviders {
		return nil, nil
	}
	return embedded.GetAll(), nil
}
