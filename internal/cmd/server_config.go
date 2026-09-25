package cmd

import (
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/workspace"
)

// serverConfig returns the full, unredacted *config.Config behind ws.
// workspace.ConfigReader.Config() only ever hands back a
// *workspace.FrontendConfig (the allowlist snapshot a UI, in-process or
// remote, is allowed to see - CLIENT-SERVER.md PR 0.5), but this package's
// login/logout/accounts/run commands run only in-process against the real
// AppWorkspace and need fields FrontendConfig deliberately drops
// (RuntimeProvider, the raw Providers map). See
// workspace.ServerConfigReader's doc comment.
//
// ws is typed workspace.Workspace (or one of its narrower role interfaces)
// at every call site, none of which declare ServerConfig, so reaching it
// takes a runtime assertion rather than a static method call. Every
// concrete Workspace this CLI ever runs against is an AppWorkspace, which
// implements ServerConfigReader; a value that doesn't (a future read-only
// or remote stand-in) makes this nil, and callers treat that exactly like
// ws.Config() returning nil today.
func serverConfig(ws workspace.ConfigReader) *config.Config {
	sc, ok := ws.(workspace.ServerConfigReader)
	if !ok {
		return nil
	}
	return sc.ServerConfig()
}
