package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pushfree/pushfree/internal/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Version:              config.SchemaVersion,
		ListenAddr:           ":0",
		CallbackAllowedHosts: []string{},
	}
	return New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func TestHealthExactBody(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Errorf("body = %q, want exact {\"status\":\"ok\"}", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
}

func TestHealthMethodNotAllowed(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	s.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("POST /health = 200, want non-200")
	}
}

func TestNotFound(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRootNotFound(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
