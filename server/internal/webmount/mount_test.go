package webmount

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux)
	return mux
}

// Acceptance: GET /admin/ -> 200, body contains <html.
func TestAdminRootServesHTML(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("body = %q, want it to contain <html", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

// Acceptance: GET /admin/foo (unknown) -> 200 index.html (SPA fallback).
func TestAdminUnknownPathSPAFallback(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/foo", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("body = %q, want index.html shell", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html on fallback", ct)
	}
}

// A nested unknown admin path also falls back (SPA client routing).
func TestAdminNestedUnknownSPAFallback(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/apps/123/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("body = %q, want index.html shell", rec.Body.String())
	}
}

// Acceptance: GET /admin/index.html -> 200, identical to /admin/.
func TestAdminIndexSameAsRoot(t *testing.T) {
	mux := newMux(t)

	root := httptest.NewRecorder()
	mux.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	idx := httptest.NewRecorder()
	mux.ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/admin/index.html", nil))

	if root.Code != http.StatusOK || idx.Code != http.StatusOK {
		t.Fatalf("status root=%d index=%d, want both 200", root.Code, idx.Code)
	}
	if root.Body.String() != idx.Body.String() {
		t.Errorf("root and index bodies differ:\n root=%q\n index=%q",
			root.Body.String(), idx.Body.String())
	}
	if !strings.Contains(idx.Body.String(), "<html") {
		t.Errorf("index body = %q, want <html", idx.Body.String())
	}
}

// Acceptance: GET /api/x -> 404 JSON error envelope, byte-stable.
func TestAPIUnknownReturnsJSON404(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got, want := rec.Body.String(), `{"status":0,"errors":["not found"]}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
}

// /api/* 404 must never return the SPA shell (misleading_success_output guard).
func TestAPINeverSPAFallback(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("API 404 returned HTML; must be JSON only: %q", rec.Body.String())
	}
}

// TestEvidenceRawBodies exists to emit raw httptest response bodies for the
// acceptance evidence file. Run with: go test -v -run TestEvidenceRawBodies.
func TestEvidenceRawBodies(t *testing.T) {
	mux := newMux(t)
	cases := []struct {
		method, target string
	}{
		{http.MethodGet, "/admin/"},
		{http.MethodGet, "/admin/foo"},
		{http.MethodGet, "/admin/index.html"},
		{http.MethodGet, "/api/x"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
		t.Logf("%s %s -> %d | content-type=%q | body=%q",
			c.method, c.target, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}
