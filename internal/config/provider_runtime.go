package config

import (
	"github.com/rave-soft/sennit/internal/csync"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
)

func (c *Config) RuntimeProvider(id string) (providerstate.Provider, bool) {
	if c == nil || c.RuntimeProviders == nil {
		return providerstate.Provider{}, false
	}
	provider, ok := c.RuntimeProviders.Get(id)
	if !ok {
		return providerstate.Provider{}, false
	}
	return providerstate.Clone(provider), true
}

func (c *Config) SetRuntimeProvider(id string, provider providerstate.Provider) {
	if c.RuntimeProviders == nil {
		c.RuntimeProviders = csync.NewMap[string, providerstate.Provider]()
	}
	c.RuntimeProviders.Set(id, providerstate.Clone(provider))
}
