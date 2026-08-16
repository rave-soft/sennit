// Package main is the entry point for the Sennit CLI.
//
//	@title			Sennit API
//	@version		1.0
//	@description	Sennit is a terminal-based AI coding assistant. This API is served over a Unix socket (or Windows named pipe) and provides programmatic access to workspaces, sessions, agents, LSP, MCP, and more.
//	@contact.name	rave-soft
//	@contact.url	https://github.com/rave-soft/sennit
//	@license.name	MIT
//	@license.url	https://github.com/rave-soft/sennit/blob/main/LICENSE
//	@BasePath		/v1
package main

import (
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"

	_ "github.com/joho/godotenv/autoload"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/cmd"
	_ "github.com/rave-soft/sennit/internal/dns"
)

func main() {
	if os.Getenv(brand.EnvPrefix+"PROFILE") != "" {
		go func() {
			slog.Info("Serving pprof at localhost:6060")
			if httpErr := http.ListenAndServe("localhost:6060", nil); httpErr != nil {
				slog.Error("Failed to pprof listen", "error", httpErr)
			}
		}()
	}

	cmd.Execute()
}
