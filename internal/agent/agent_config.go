package agent

import (
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/hooks"
)

// agentConfig is the slice of *config.Config that buildTools needs to
// assemble a tool set — a fixed list of values instead of buildTools
// reaching into config.Config's full shape (Options.Attribution,
// Tools.Glob, Hooks[event], ...) field by field. newAgentConfig builds
// the only implementation from a single c.cfg.Config() read, so
// buildTools can take one snapshot per build and hand it to every tool it
// constructs.
type agentConfig interface {
	// ModelID is the selected model's catalog ID, or "" if none is
	// selected or it doesn't resolve against the configured providers.
	ModelID() string
	PreToolUseHooks() []config.HookConfig
	Attribution() *config.Attribution
	SkillsPaths() []string
	Glob() config.ToolGlob
	Grep() config.ToolGrep
	Ls() config.ToolLs
	// DataDirectory is the project-local directory for workspace-scoped
	// state (Options.DataDirectory).
	DataDirectory() string
	HasLSP() bool
	// AutoLSPEnabled reports whether options.auto_lsp allows automatic
	// LSP setup: unset (nil) defaults to enabled.
	AutoLSPEnabled() bool
	HasMCP() bool
	Agents() map[string]config.Agent
}

// configSnapshot is agentConfig's implementation: a plain value copy of
// the fields buildTools reads, taken once per build.
type configSnapshot struct {
	modelID         string
	preToolUseHooks []config.HookConfig
	attribution     *config.Attribution
	skillsPaths     []string
	glob            config.ToolGlob
	grep            config.ToolGrep
	ls              config.ToolLs
	dataDirectory   string
	hasLSP          bool
	autoLSP         bool
	hasMCP          bool
	agents          map[string]config.Agent
}

func newAgentConfig(cfg *config.Config) agentConfig {
	modelID := ""
	if selected := cfg.Model; selected.Model != "" {
		if model := cfg.GetModel(selected.Provider, selected.Model); model != nil {
			modelID = model.ID
		}
	}
	return &configSnapshot{
		modelID:         modelID,
		preToolUseHooks: cfg.Hooks[hooks.EventPreToolUse],
		attribution:     cfg.Options.Attribution,
		skillsPaths:     cfg.Options.SkillsPaths,
		glob:            cfg.Tools.Glob,
		grep:            cfg.Tools.Grep,
		ls:              cfg.Tools.Ls,
		dataDirectory:   cfg.Options.DataDirectory,
		hasLSP:          len(cfg.LSP) > 0,
		autoLSP:         cfg.Options.AutoLSP == nil || *cfg.Options.AutoLSP,
		hasMCP:          len(cfg.MCP) > 0,
		agents:          cfg.Agents,
	}
}

func (s *configSnapshot) ModelID() string                      { return s.modelID }
func (s *configSnapshot) PreToolUseHooks() []config.HookConfig { return s.preToolUseHooks }
func (s *configSnapshot) Attribution() *config.Attribution     { return s.attribution }
func (s *configSnapshot) SkillsPaths() []string                { return s.skillsPaths }
func (s *configSnapshot) Glob() config.ToolGlob                { return s.glob }
func (s *configSnapshot) Grep() config.ToolGrep                { return s.grep }
func (s *configSnapshot) Ls() config.ToolLs                    { return s.ls }
func (s *configSnapshot) DataDirectory() string                { return s.dataDirectory }
func (s *configSnapshot) HasLSP() bool                         { return s.hasLSP }
func (s *configSnapshot) AutoLSPEnabled() bool                 { return s.autoLSP }
func (s *configSnapshot) HasMCP() bool                         { return s.hasMCP }
func (s *configSnapshot) Agents() map[string]config.Agent      { return s.agents }
