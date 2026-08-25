package config

import (
	"context"
	"os"

	"charm.land/catwalk/pkg/catwalk"
)

type RuntimeProcessor interface {
	Process(context.Context, RuntimeInput) (RuntimeResult, error)
}

type RuntimeInput struct {
	Config          *Config
	Store           RuntimeStore
	GlobalDataPath  string
	CredentialsHome string
	Stat            func(string) (os.FileInfo, error)
	Initial         bool
	KnownProviders  []catwalk.Provider
}

type RuntimeResult struct {
	KnownProviders []catwalk.Provider
	Resolver       VariableResolver
}

type RuntimeStore interface {
	RemoveRuntimeConfigField(Scope, string)
	WriteRuntimeConfigFields(Scope, map[string]any)
}

func (c *Config) ApplyRuntimeEnvironment(resolver VariableResolver) {
	c.applyEnv(resolver)
}

func (c *Config) AddRuntimeProblem(problem Problem) {
	c.addProblem(problem)
}
