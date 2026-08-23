// Package main is the entry point for the Sennit CLI.
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
