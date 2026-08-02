// Command pushfree starts the pushfree server.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/pushfree/pushfree/internal/api"
	"github.com/pushfree/pushfree/internal/config"
	"github.com/pushfree/pushfree/internal/server"
	"github.com/pushfree/pushfree/internal/store/sqlite"
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

	st, err := sqlite.Open(ctx, cfg.DBFile)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	// Stateful sessions need a stable signing secret; a config secret persists
	// across restarts. With none set, a random one is generated per process and
	// all outstanding sessions are invalidated on the next restart.
	authSecret := []byte(cfg.AuthSecret)
	if len(authSecret) == 0 {
		gen := make([]byte, 32)
		if _, err := rand.Read(gen); err != nil {
			return fmt.Errorf("generate auth secret: %w", err)
		}
		authSecret = gen
		logger.Warn("auth-secret is not configured; generated a random session secret that will not survive restarts. Set \"auth-secret\" in the config or PUSHFREE_AUTH_SECRET to persist sessions")
	}

	srv := server.New(cfg, logger)
	api.New(st.Repos(), authSecret, 0, logger).Register(srv.Mux())
	logger.Info("starting pushfree server", "listen-addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "")
	err = srv.Run(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
