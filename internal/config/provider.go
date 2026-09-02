package config

import (
	providerconfig "github.com/rave-soft/sennit/internal/providers/config"
)

// ProviderConfig, RotationConfig, and ModelsSource live in
// internal/providers/config now (see that package's doc comment for why:
// internal/providers/runtime needs them, and internal/config needs to
// call into internal/providers/runtime directly, so they cannot live in
// internal/config without recreating an import cycle). Aliased here so
// the rest of the tree can keep saying config.ProviderConfig.
type ProviderConfig = providerconfig.ProviderConfig

type RotationConfig = providerconfig.RotationConfig

type ModelsSource = providerconfig.ModelsSource

const (
	ModelsSourceConfig = providerconfig.ModelsSourceConfig
	ModelsSourceCache  = providerconfig.ModelsSourceCache
)
