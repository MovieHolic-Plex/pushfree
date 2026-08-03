package webmount

import (
	"io/fs"
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

// firstGlob returns the first embedded path matching pattern, skipping the
// test if none exists (the export layout changed). Tests use it so they do
// not hardcode content-hashed file names that change every rebuild.
func firstGlob(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := fs.Glob(assets, pattern)
	if err != nil {
		t.Fatalf("glob %q: %v", pattern, err)
	}
	if len(matches) == 0 {
		t.Skipf("no embedded asset matches %q; export layout changed", pattern)
	}
	return matches[0]
}

// adminRelToURL converts an embedded asset path like
// "web/out/_next/static/chunks/foo.js" into the request URL that serves it
// ("/admin/_next/static/chunks/foo.js").
func adminRelToURL(t *testing.T, embedded string) string {
	t.Helper()
	const prefix = "web/out/"
	if !strings.HasPrefix(embedded, prefix) {
		t.Fatalf("asset %q does not start with %q", embedded, prefix)
	}
	return "/admin/" + strings.TrimPrefix(embedded, prefix)
}

// Acceptance: GET /admin/ -> 200, body contains <html, and is NOT the
// placeholder shell (the real export is embedded).
func TestAdminRootServesRealHTML(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<html") {
		t.Errorf("body = %q, want it to contain <html", body)
	}
	if strings.Contains(strings.ToLower(body), "placeholder") {
		t.Errorf("body still contains placeholder text; real export not embedded: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	// HTML must be revalidated, not cached immutably.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("cache-control = %q, want no-cache for HTML", cc)
	}
}

// Acceptance (manual QA happy path): GET /admin/dashboard serves the real,
// pre-rendered dashboard page (clean URL -> dashboard.html), not the index
// shell and not the placeholder.
func TestAdminDashboardServesRealPage(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<html") {
		t.Errorf("body = %q, want <html", body)
	}
	// dashboard.html references its route chunk; the index shell does not.
	if !strings.Contains(body, "app/dashboard/page") {
		t.Errorf("body = %q, want dashboard route chunk reference", body)
	}
	if strings.Contains(strings.ToLower(body), "placeholder") {
		t.Errorf("body contains placeholder text; real export not embedded: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

// Nested clean URL: /admin/dashboard/apps -> dashboard/apps.html.
func TestAdminNestedCleanURL(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/dashboard/apps", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "app/dashboard/apps/page") {
		t.Errorf("body = %q, want apps route chunk reference", body)
	}
}

// MIME correctness: a hashed JS chunk is served as text/javascript (NOT
// text/plain, which http.DetectContentType would return and browsers would
// refuse to execute).
func TestAdminJSMimeType(t *testing.T) {
	js := firstGlob(t, "web/out/_next/static/chunks/*.js")
	target := adminRelToURL(t, js)

	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for %s", rec.Code, target)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("content-type for %s = %q, want text/javascript (MIME sniffing bug?)", target, ct)
	}
	// Hashed assets are immutable.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("cache-control = %q, want immutable for %s", cc, target)
	}
}

// MIME correctness: a CSS bundle is served as text/css.
func TestAdminCSSMimeType(t *testing.T) {
	css := firstGlob(t, "web/out/_next/static/css/*.css")
	target := adminRelToURL(t, css)

	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for %s", rec.Code, target)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type for %s = %q, want text/css", target, ct)
	}
}

// Acceptance (manual QA failure path): an unknown extensionless subpath
// falls back to index.html (200) so the SPA client router can resolve it.
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

// A missing asset with an extension must 404, not return the HTML shell with
// a text/html content-type (misleading_success_output guard).
func TestAdminMissingAssetIs404(t *testing.T) {
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/_next/static/chunks/does-not-exist.js", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for missing asset", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("missing asset returned HTML shell; want a plain 404: %q", rec.Body.String())
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

// TestEvidenceRawBodies emits raw httptest response heads+bodies for the
// acceptance evidence file. Run with: go test -v -run TestEvidenceRawBodies.
func TestEvidenceRawBodies(t *testing.T) {
	mux := newMux(t)
	jsTarget := adminRelToURL(t, firstGlob(t, "web/out/_next/static/chunks/framework-*.js"))
	cases := []struct{ method, target string }{
		{http.MethodGet, "/admin/"},
		{http.MethodGet, "/admin/dashboard"},
		{http.MethodGet, "/admin/foo"},
		{http.MethodGet, "/admin/index.html"},
		{http.MethodGet, jsTarget},
		{http.MethodGet, "/admin/_next/static/chunks/missing.js"},
		{http.MethodGet, "/api/x"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
		t.Logf("%s %s -> %d | content-type=%q | cache-control=%q | body=%q",
			c.method, c.target, rec.Code,
			rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"),
			rec.Body.String())
	}
}
