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

	"github.com/pushfree/pushfree/internal/config"
	"github.com/pushfree/pushfree/internal/webmount"
)

// Server is the pushfree HTTP server. It embeds an http.Handler (the mux) so
// it satisfies http.Handler directly, which keeps both httptest-based unit
// tests and the live http.Server simple.
type Server struct {
	cfg    *config.Config
	logger *slog.Logger
	mux    *http.ServeMux
	http.Handler
	srv *http.Server
}

// New builds a Server with the dependency-free routes registered: the
// /health liveness probe and the webmount dashboard (/admin/ embed +
// /api/ JSON 404 envelope). Route groups that need external collaborators
// (the accounts API under /v1/, which needs the store Repos and auth
// secret) are mounted by the caller on Mux() after construction; that keeps
// this package free of store/secret wiring.
func New(cfg *config.Config, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	webmount.Register(mux)
	return &Server{
		cfg:     cfg,
		logger:  logger,
		mux:     mux,
		Handler: mux,
	}
}

// Mux returns the root ServeMux so callers can register additional route
// groups after construction without disturbing /health.
func (s *Server) Mux() *http.ServeMux { return s.mux }

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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	}
}
