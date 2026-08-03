// Package server wires the pushfree HTTP server: routing, TLS, and graceful
// shutdown. It intentionally contains no business logic yet.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/pushfree/pushfree/internal/api"
	"github.com/pushfree/pushfree/internal/config"
	"github.com/pushfree/pushfree/internal/hub"
	"github.com/pushfree/pushfree/internal/metrics"
	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/up"
	"github.com/pushfree/pushfree/internal/webmount"
)

// Server is the pushfree HTTP server. It embeds an http.Handler (the
// request-logging-wrapped mux) so it satisfies http.Handler directly, which
// keeps both httptest-based unit tests and the live http.Server simple.
type Server struct {
	cfg    *config.Config
	logger *slog.Logger
	mux    *http.ServeMux
	http.Handler
	srv     *http.Server
	hub     *hub.Hub         // nil until MountRealtime; closed on graceful shutdown
	metrics *metrics.Metrics // pushfree_* collectors; exposed via Metrics()
}

// New builds a Server with the dependency-free routes registered: the
// /health liveness probe, the /metrics Prometheus endpoint (todo 15), and
// the webmount dashboard (/admin/ embed + /api/ JSON 404 envelope). The mux
// is wrapped by the request-logging middleware, which assigns a per-request
// id (echoed in X-Request-ID and the slog line) and records the send/
// messages-received/ws-clients observations. Route groups that need external
// collaborators (the accounts API under /v1/, which needs the store Repos
// and auth secret) are mounted by the caller on Mux() after construction;
// because the middleware wraps the mux (not a snapshot), late-registered
// routes still flow through it.
func New(cfg *config.Config, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	bundle := metrics.NewBundle()
	mux.Handle("GET /metrics", bundle.Handler)
	webmount.Register(mux)
	return &Server{
		cfg:     cfg,
		logger:  logger,
		mux:     mux,
		metrics: bundle.Metrics,
		Handler: metrics.RequestLogger(logger, bundle.Metrics)(mux),
	}
}

// Mux returns the root ServeMux so callers can register additional route
// groups after construction without disturbing /health or /metrics.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// Metrics returns the pushfree_* collectors so other server workers (hub,
// receipts, transports) can record delivery/ack observations without each
// owning a registry. It is never nil for a Server built by New.
func (s *Server) Metrics() *metrics.Metrics { return s.metrics }

// MountRealtime wires the Open Client transports and the UnifiedPush
// distributor into the root mux. It builds the REAL session resolver
// (api.SessionResolver, the signed-cookie implementation from security.go) so
// device registration under /1/devices/login.json and /up/{sub}/subscribe.json
// share one session identity. The hub owns the four /1/* Open Client routes;
// the UP handler owns the three /up/{sub}/* distributor routes. Keepalive is
// parsed from the configured string (defaulting to 45s on a malformed value).
// The hub is retained so Run can close it during graceful shutdown.
func (s *Server) MountRealtime(repos store.Repos, authSecret []byte) {
	resolver := api.NewSessionResolver(authSecret)
	h := hub.New(repos, resolver, hub.Options{
		KeepaliveInterval: parseKeepalive(s.cfg.KeepaliveInterval),
		Logger:            s.logger,
	})
	s.hub = h
	mux := s.mux
	mux.HandleFunc("POST /1/devices/login.json", h.ServeDeviceLogin)
	mux.HandleFunc("GET /1/messages.json", h.GetMessagesHandler)
	mux.HandleFunc("GET /1/ws", h.ServeWS)
	mux.HandleFunc("GET /1/sse", h.ServeSSE)
	up.New(repos, resolver, authSecret, up.Options{Logger: s.logger}).Register(mux)
}

// parseKeepalive resolves the configured keepalive duration, defaulting to
// the documented 45s on a missing/malformed value so a bad config string can
// never disable keepalives (which would silently drop idle clients behind
// proxies).
func parseKeepalive(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return 45 * time.Second
}

// health reports liness. The body is exactly {"status":"ok"} with no trailing
// newline so callers can assert byte-for-byte equality.
func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

// Run starts serving and blocks until ctx is canceled (SIGINT/SIGTERM in
// production) or the listener fails. On ctx cancellation it performs a
// graceful shutdown capped at 10 seconds.
func (s *Server) Run(ctx context.Context) error {
	s.srv = &http.Server{
		Addr:    s.cfg.ListenAddr,
		Handler: s,
	}

	useTLS := false
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("load TLS key pair: %w", err)
		}
		s.srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		useTLS = true
	}

	errCh := make(chan error, 1)
	go func() {
		if useTLS {
			errCh <- s.srv.ListenAndServeTLS("", "")
		} else {
			errCh <- s.srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		if s.hub != nil {
			s.hub.Close() // signal live WS/SSE loops to wind down
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	}
}
