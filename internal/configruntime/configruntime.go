package configruntime

import (
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providerload"
)

func Load(workingDir, dataDir string, debug bool) (*config.ConfigStore, error) {
	return LoadWithProcessor(workingDir, dataDir, debug, providerload.New())
}

func LoadWithProcessor(workingDir, dataDir string, debug bool, processor config.RuntimeProcessor) (*config.ConfigStore, error) {
	return config.LoadWithProcessor(workingDir, dataDir, debug, processor)
}
