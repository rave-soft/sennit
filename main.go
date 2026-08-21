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
	"net/http/pprof"
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
			mux := http.NewServeMux()
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			if httpErr := http.ListenAndServe("localhost:6060", mux); httpErr != nil {
				slog.Error("Failed to pprof listen", "error", httpErr)
			}
		}()
	}

	cmd.Execute()
}
