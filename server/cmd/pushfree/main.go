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
	"github.com/pushfree/pushfree/internal/fcm"
	"github.com/pushfree/pushfree/internal/receipts"
	"github.com/pushfree/pushfree/internal/retention"
	"github.com/pushfree/pushfree/internal/server"
	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/store/postgres"
	"github.com/pushfree/pushfree/internal/store/sqlite"
	"github.com/pushfree/pushfree/internal/timers"
)

func main() {
	if err := run(); err != nil {
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

	// --- Database backend selection ---------------------------------------
	// If db-url is set, use Postgres; otherwise SQLite (the default).
	repos, timerStore, retryStore, receiptGC, callbackWorkerRepo, cancelRepo, runRetension, walCp, closeDB, err := openStore(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer closeDB()

	// --- Sweepers (message retention + receipt 7-day GC) ------------------
	sweeper, err := retention.NewSweeper(runRetension, retention.SystemClock{},
		cfg.SweeperInterval, cfg.MessagesRetention, cfg.AttachmentRetention, logger)
	if err != nil {
		return fmt.Errorf("retention sweeper: %w", err)
	}
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		_ = sweeper.Run(ctx)
	}()

	sweeperInterval := parseDurationOrDefault(cfg.SweeperInterval, time.Hour)

	receiptSweeper := api.NewReceiptSweeper(receiptGC, 7*24*time.Hour, time.Now, sweeperInterval)
	receiptSweeperDone := make(chan struct{})
	go func() {
		defer close(receiptSweeperDone)
		receiptSweeper.Run(ctx)
	}()

	// --- Timer engine (priority-2 retry/expire) ---------------------------
	timerEngine := timers.NewEngine(timerStore)
	timerDone := make(chan struct{})

	if cfg.ShutdownOnStdinEOF {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			logger.Info("stdin eof; initiating graceful shutdown")
			stop()
		}()
	}

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

	// --- Callback worker --------------------------------------------------
	callbackWorker := callbacks.NewWorker(callbackWorkerRepo, callbacks.Options{
		AllowedHosts: cfg.CallbackAllowedHosts,
		Logger:       logger,
	})
	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		_ = callbackWorker.Run(ctx)
	}()

	// --- Accounts + routes -----------------------------------------------
	accounts := api.New(repos, authSecret, 0, logger)
	accounts.SetAckHook(callbackWorker)

	// Wire the hub first so the redeliver adapter can reference it.
	srv.MountRealtime(repos, authSecret)
	h := srv.Hub()

	// Build the redeliver adapter and retry handler.
	redeliver := server.NewRedeliverFunc(repos, h, logger)
	timerEngine.RegisterHandler(timers.KindRetry, timers.NewRetryHandler(
		timerEngine, retryStore, receipts.DefaultRetryPolicy(),
		timers.Clock(time.Now), redeliver, nil,
	))

	// Wire the retry seeder so priority-2 sends create their initial timer.
	seeder := server.NewRetrySeeder(timerEngine, receipts.DefaultRetryPolicy(), logger)
	accounts.SetRetrySeeder(seeder)

	// Start the timer engine loop.
	go func() {
		defer close(timerDone)
		_ = timerEngine.Run(ctx, sweeperInterval)
	}()

	accounts.Register(srv.Mux())
	api.NewCancelAPI(repos, cancelRepo, nil, logger).Register(srv.Mux())
	accounts.SetLivePublisher(h)

	// --- FCM (optional) ---------------------------------------------------
	if cfg.FCMCredentialsFile != "" {
		fcmClient, err := fcm.MaybeNew(ctx, cfg.FCMCredentialsFile, logger)
		if err != nil {
			logger.Error("fcm: failed to initialize, channel disabled", "err", err)
		} else if fcmClient != nil {
			logger.Info("fcm: delivery channel enabled")
		} else {
			logger.Info("fcm: delivery channel disabled (no credentials)")
		}
	} else {
		logger.Info("fcm: delivery channel disabled (no credentials)")
	}

	logger.Info("starting pushfree server", "listen-addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "")
	err = srv.Run(ctx)

	// --- Graceful shutdown ------------------------------------------------
	stop()
	<-sweeperDone
	<-receiptSweeperDone
	<-callbackDone
	<-timerDone

	runWALCheckpoint(ctx, logger, walCp, cfg.ShutdownTimeout)

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// openStore opens the configured database backend (SQLite or Postgres based
// on cfg.DBURL) and returns the wired components main.go needs.
func openStore(ctx context.Context, cfg *config.Config, logger *slog.Logger) (
	repos store.Repos,
	timerStore timers.Store,
	retryStore receipts.RetryStore,
	receiptGC api.ReceiptGCStore,
	callbackWorkerRepo callbacks.Store,
	cancelRepo receipts.CancelStore,
	retentionStore retention.Store,
	walCp func(context.Context) (sqlite.WALCheckpointResult, error),
	closeFn func(),
	err error,
) {
	if cfg.DBURL != "" {
		pgSt, e := postgres.Open(ctx, cfg.DBURL)
		if e != nil {
			err = fmt.Errorf("open postgres: %w", e)
			return
		}
		repos = pgSt.Repos()
		timerStore = pgSt.TimerEngine()
		retryStore = pgSt.ReceiptRepo()
		receiptGC = pgSt.Repos().Receipts
		callbackWorkerRepo = postgres.NewCallbackWorkerRepo(pgSt.DB())
		cancelRepo = postgres.NewCancelRepo(pgSt.DB())
		retentionStore = pgSt
		walCp = func(context.Context) (sqlite.WALCheckpointResult, error) {
			return sqlite.WALCheckpointResult{}, nil
		}
		closeFn = func() { _ = pgSt.Close() }
		return
	}

	sqliteSt, e := sqlite.Open(ctx, cfg.DBFile)
	if e != nil {
		err = fmt.Errorf("open sqlite: %w", e)
		return
	}
	logger.Info("database backend: sqlite", "db-file", cfg.DBFile)
	repos = sqliteSt.Repos()
	timerStore = sqliteSt.TimerEngine()
	retryStore = sqliteSt.ReceiptRepo()
	receiptGC = sqliteSt.Repos().Receipts
	callbackWorkerRepo = sqlite.NewCallbackWorkerRepo(sqliteSt.DB())
	cancelRepo = sqlite.NewCancelRepo(sqliteSt.DB())
	retentionStore = sqliteSt
	walCp = sqliteSt.WALCheckpoint
	closeFn = func() { _ = sqliteSt.Close() }
	return
}

func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

func runWALCheckpoint(ctx context.Context, logger *slog.Logger, cp func(context.Context) (sqlite.WALCheckpointResult, error), timeout string) {
	dur, err := time.ParseDuration(timeout)
	if err != nil {
		logger.Error("invalid shutdown-timeout; using fallback", "raw", timeout, "err", err)
		dur = 10 * time.Second
	}
	cpCtx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	res, cpErr := cp(cpCtx)
	if cpErr != nil {
		logger.Error("shutdown wal checkpoint failed", "err", cpErr)
	} else {
		logger.Info("shutdown wal checkpoint complete",
			"busy", res.Busy, "log", res.Log, "checkpointed", res.Checkpointed)
	}
}
