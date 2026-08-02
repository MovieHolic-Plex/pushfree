// Command pushfree starts the pushfree server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/pushfree/pushfree/internal/config"
	"github.com/pushfree/pushfree/internal/server"
)

func main() {
	if err := run(); err != nil {
		// Clear, human-readable message on stderr; non-zero exit so
		// supervisors/containers see a failure.
		fmt.Fprintln(os.Stderr, "pushfree:", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to TOML config file (optional)")
	flag.Parse()

	cfg, err := config.Load(configPath, os.LookupEnv)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
		logger.Warn("TLS is not configured; serving plain HTTP. Run pushfree behind a TLS-terminating reverse proxy in production", "listen-addr", cfg.ListenAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg, logger)
	logger.Info("starting pushfree server", "listen-addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "")
	err = srv.Run(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
