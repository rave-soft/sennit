// Package devtools holds diagnostics that are compiled into every build but
// stay dormant unless explicitly switched on.
package devtools

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"time"
)

// PprofEnvVar names the environment variable that switches the profiling
// server on. Its value is the address to listen on, e.g. "localhost:6060".
const PprofEnvVar = "SENNIT_PPROF"

// pprofReadHeaderTimeout bounds how long a client may take to send its
// request headers. It exists to satisfy the linter's insistence on a
// non-default server timeout rather than to defend anything: the listener
// is expected to be bound to loopback.
const pprofReadHeaderTimeout = 5 * time.Second

// StartPprof starts net/http/pprof on the address in SENNIT_PPROF. It
// returns the address actually bound — which is not the requested one when
// that asked for port 0 — and a function that shuts the server down. When
// the variable is unset, nothing is started: the address is empty and the
// returned function does nothing.
//
// Sennit is a TUI: when it misbehaves it does so as a wedged render loop or
// a goroutine pile-up, and neither leaves anything in the log. Without an
// endpoint the only ways in are a SIGQUIT (which kills the session being
// diagnosed) or attaching a debugger, which on a stock Linux needs root
// because ptrace_scope defaults to 1. Both are poor answers to "it is
// burning a core right now, what is it doing". This is the cheap answer:
//
//	SENNIT_PPROF=localhost:6060 sennit
//	go tool pprof -top http://localhost:6060/debug/pprof/profile?seconds=10
//	curl -s http://localhost:6060/debug/pprof/goroutine?debug=1 | head -40
//
// The address is taken from the environment rather than defaulted so that
// nothing is listening unless someone asked for it. Bind it to loopback:
// the profiles carry function names, and the endpoint can be used to make
// the process do work.
func StartPprof() (addr string, stop func()) {
	requested := os.Getenv(PprofEnvVar)
	if requested == "" {
		return "", func() {}
	}

	// A dedicated mux, not http.DefaultServeMux: importing net/http/pprof
	// for its side effect would register these handlers process-wide, on a
	// mux any other package may also be serving.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", requested)
	if err != nil {
		slog.Error("Could not start the pprof server", "addr", requested, "error", err)
		return "", func() {}
	}
	addr = listener.Addr().String()

	// Block profiling is off by default because it costs on every blocking
	// operation. It is switched on here because the whole point of asking
	// for this server is that something is stuck, and "which channel are
	// 900 goroutines parked on" is the question that answers it.
	runtime.SetBlockProfileRate(1000)
	runtime.SetMutexProfileFraction(100)

	server := &http.Server{Handler: mux, ReadHeaderTimeout: pprofReadHeaderTimeout}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("The pprof server stopped", "error", err)
		}
	}()
	slog.Info("Started the pprof server", "addr", addr)

	return addr, func() {
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
		_ = server.Close()
	}
}
