package cmd

import (
	"log/slog"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/workspace"
)

// serverConfig returns the full, unredacted *config.Config behind ws.
// workspace.ConfigReader.Config() only ever hands back a
// *workspace.FrontendConfig (the allowlist snapshot a UI, in-process or
// remote, is allowed to see - CLIENT-SERVER.md PR 0.5). See
// workspace.ServerConfigReader's doc comment.
//
// ws is typed workspace.Workspace (or one of its narrower role interfaces)
// at every call site, none of which declare ServerConfig, so reaching it
// takes a runtime assertion rather than a static method call. Only an
// in-process *appws.AppWorkspace implements ServerConfigReader; a
// *grpcws.Client (daemon mode) or a future read-only stand-in makes this
// nil, and callers treat that exactly like ws.Config() returning nil.
//
// Every daemon-reachable call site in this package has moved off this
// function (CLIENT-SERVER.md PR 2.3): login/logout/accounts read
// ws.Config() (*FrontendConfig) or a narrow Workspace method instead, and
// run.go's overrideModel (-m/--model) reads ws.Config() via
// modelMatchProviders instead of this, after a plain `sennit run -m ...`
// against a running daemon (routed there since 1e5d36339) nil-deref
// panicked on this returning nil for a *grpcws.Client. The one remaining
// call, runDisplayConfig below, is still safe against a daemon
// connection: it treats this returning nil as "not in-process" and
// falls back to a client-side config load rather than assuming an
// in-process *config.Config is available.
func serverConfig(ws workspace.ConfigReader) *config.Config {
	sc, ok := ws.(workspace.ServerConfigReader)
	if !ok {
		return nil
	}
	return sc.ServerConfig()
}

// runDisplayConfig resolves the *config.Config `sennit run` reads its
// spinner, theme and progress-bar preferences from (CLIENT-SERVER.md, PR
// 2.3). In-process, that's the real config already loaded behind ws
// (serverConfig). Against a daemon (ws is a *grpcws.Client - ServerConfig
// deliberately never crosses the wire, see serverConfig's doc comment
// above), it loads the same client-side config `sennit attach` and the
// daemon TUI's own UI preferences already read (config.LoadData):
// read-only, local to this machine, and merged the same way, so a `sennit
// run` through the daemon looks the same as one that ran in-process.
func runDisplayConfig(ws workspace.Workspace, cwd, dataDir string, debug bool) *config.Config {
	if cfg := serverConfig(ws); cfg != nil {
		return cfg
	}
	cfgStore, err := config.LoadData(cwd, dataDir, debug)
	if err != nil {
		slog.Debug("Failed to load local config for display preferences", "error", err)
		return nil
	}
	return cfgStore.Config()
}
