// Command pushfree starts the pushfree server.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pushfree/pushfree/internal/api"
	"github.com/pushfree/pushfree/internal/callbacks"
	"github.com/pushfree/pushfree/internal/config"
	"github.com/pushfree/pushfree/internal/retention"
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

	// Retention sweeper: hourly message/attachment/TTL cleanup. A malformed
	// duration fails loudly here rather than at the first sweep.
	sweeper, err := retention.NewSweeper(st, retention.SystemClock{},
		cfg.SweeperInterval, cfg.MessagesRetention, cfg.AttachmentRetention, logger)
	if err != nil {
		return fmt.Errorf("retention sweeper: %w", err)
	}
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		_ = sweeper.Run(ctx)
	}()

	// Windows/testing graceful-stop path. POSIX SIGTERM cannot be delivered
	// to a Windows console child process (os.Process.Signal is unsupported
	// and taskkill posts WM_CLOSE, which console apps never see), so when
	// this flag is set, closing stdin cancels the root context exactly like
	// SIGTERM/SIGINT. Leave false in normal operation.
	if cfg.ShutdownOnStdinEOF {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			logger.Info("stdin eof; initiating graceful shutdown")
			stop()
		}()
	}

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

	// Callback worker (todo 25): SSRF-guarded webhook delivery with a 60s
	// retry on non-2xx. Built before Accounts so the ack-hook seam is
	// installed before routes are registered. The default denylist (loopback,
	// link-local, RFC1918, ULA) is fully enforced; only configured
	// callback-allowed-hosts bypass it.
	callbackWorker := callbacks.NewWorker(sqlite.NewCallbackWorkerRepo(st.DB()), callbacks.Options{
		AllowedHosts: cfg.CallbackAllowedHosts,
		Logger:       logger,
	})
	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		_ = callbackWorker.Run(ctx)
	}()

	accounts := api.New(st.Repos(), authSecret, 0, logger)
	accounts.SetAckHook(callbackWorker)
	accounts.Register(srv.Mux())
	// Cancel + cancel_by_tag endpoints (todo 24). Registered as a standalone
	// group on the same mux; the CancelStore is the SQLite CancelRepo over the
	// shared DB. The live cancel broadcaster is nil here -- the hub-side
	// BroadcastCancel is wired once live message push exists (todos 22/23);
	// the persisted canceled state stops retries regardless.
	api.NewCancelAPI(st.Repos(), sqlite.NewCancelRepo(st.DB()), nil, logger).Register(srv.Mux())
	srv.MountRealtime(st.Repos(), authSecret)
	logger.Info("starting pushfree server", "listen-addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "")
	err = srv.Run(ctx)

	// server.Run has drained HTTP (capped at 10s internally on ctx cancel).
	// Release the sweeper (on a startup-bind failure ctx is not yet canceled)
	// and wait for it to stop touching the database before the checkpoint.
	stop()
	<-sweeperDone
	<-callbackDone

	// WAL checkpoint as the shutdown tail: bounded by shutdown-timeout so the
	// whole stop stays inside the 10s budget on an idle server. The logged
	// result is the WAL-checkpoint evidence required by the SIGTERM test.
	shutdownTimeout, perr := time.ParseDuration(cfg.ShutdownTimeout)
	if perr != nil {
		logger.Error("invalid shutdown-timeout; using fallback", "raw", cfg.ShutdownTimeout, "err", perr)
		shutdownTimeout = 10 * time.Second
	}
	checkpointCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	res, cpErr := st.WALCheckpoint(checkpointCtx)
	cancel()
	if cpErr != nil {
		logger.Error("shutdown wal checkpoint failed", "err", cpErr)
	} else {
		logger.Info("shutdown wal checkpoint complete",
			"busy", res.Busy, "log", res.Log, "checkpointed", res.Checkpointed)
	}

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
