package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/hostaddr"
	braidlog "github.com/rave-soft/braid/internal/log"
	"github.com/rave-soft/braid/internal/server"
	"github.com/spf13/cobra"
)

var serverHost string

func init() {
	serverCmd.Flags().StringVarP(&serverHost, "host", "H", hostaddr.DefaultHost(), "Server host (TCP or Unix socket)")
	rootCmd.AddCommand(serverCmd)
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the Braid server",
	RunE: func(cmd *cobra.Command, _ []string) error {
		dataDir, err := cmd.Flags().GetString("data-dir")
		if err != nil {
			return fmt.Errorf("failed to get data directory: %v", err)
		}
		debug, err := cmd.Flags().GetBool("debug")
		if err != nil {
			return fmt.Errorf("failed to get debug flag: %v", err)
		}

		cfg, err := config.Load(config.GlobalWorkspaceDir(), dataDir, debug)
		if err != nil {
			return fmt.Errorf("failed to load configuration: %v", err)
		}

		hostURL, err := hostaddr.ParseHostURL(serverHost)
		if err != nil {
			return fmt.Errorf("invalid server host: %v", err)
		}

		logFile := filepath.Join(config.GlobalCacheDir(), "server-"+safeHostName(hostURL), "braid.log")

		if term.IsTerminal(os.Stderr.Fd()) {
			braidlog.Setup(logFile, debug, os.Stderr)
		} else {
			braidlog.Setup(logFile, debug)
		}

		srv := server.NewServer(cfg, hostURL.Scheme, hostURL.Host)
		srv.SetLogger(slog.Default())
		slog.Info("Starting Braid server...", "addr", serverHost)

		errch := make(chan error, 1)
		sigch := make(chan os.Signal, 1)
		sigs := []os.Signal{os.Interrupt}
		sigs = append(sigs, addSignals(sigs)...)
		signal.Notify(sigch, sigs...)

		go func() {
			errch <- srv.ListenAndServe()
		}()

		select {
		case <-sigch:
			slog.Info("Received interrupt signal...")
		case err = <-errch:
			if err != nil && !errors.Is(err, server.ErrServerClosed) {
				_ = srv.Close()
				slog.Error("Server error", "error", err)
				return fmt.Errorf("server error: %v", err)
			}
		}

		if errors.Is(err, server.ErrServerClosed) {
			return nil
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()

		slog.Info("Shutting down...")

		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("Failed to shutdown server", "error", err)
			return fmt.Errorf("failed to shutdown server: %v", err)
		}

		return nil
	},
}
