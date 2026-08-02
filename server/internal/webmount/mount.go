package webmount

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const (
	// adminPrefix is the subtree the dashboard is served under.
	adminPrefix = "/admin/"
	// assetRoot is the embedded directory (relative to this package) holding
	// the export, matching the go:embed directive in embed.go.
	assetRoot = "web/out"
	// indexName is the SPA shell served on directory and fallback requests.
	indexName = "index.html"
)

// errorEnvelope is the Pushover-compatible error shape used for API 404s.
// Field order is fixed so the JSON body is byte-stable.
type errorEnvelope struct {
	Status int      `json:"status"`
	Errors []string `json:"errors"`
}

// notFoundBody is the canonical JSON for an unmatched API route. It is
// precomputed so the 404 path allocates nothing on the hot path.
var notFoundBody = mustMarshal(errorEnvelope{Status: 0, Errors: []string{"not found"}})

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// errorEnvelope only contains concrete, encodable types.
		panic("webmount: cannot marshal not-found envelope: " + err.Error())
	}
	return b
}

// Register mounts the dashboard and the API 404 envelope onto mux.
//
//	Route behaviour:
//	  GET /admin/*  embedded static file; a missing asset falls back to
//	                index.html with 200 so client-side SPA routing works.
//	  /api/*        JSON 404 envelope (any method), never SPA-fallback.
//
// More specific routes registered elsewhere (e.g. the real API under /1/ and
// /v1/, or explicit /api/... handlers) outrank the /api/ subtree by ServeMux
// specificity, so this never shadows them.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+adminPrefix, serveAdmin)
	mux.HandleFunc("/api/", serveAPINotFound)
}

// serveAdmin serves an embedded asset, falling back to the SPA shell for any
// path that does not resolve to a real file.
func serveAdmin(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, adminPrefix)
	if name == "" {
		name = indexName
	}

	data, err := fs.ReadFile(assets, path.Join(assetRoot, name))
	if err == nil {
		w.Header().Set("Content-Type", contentType(name, data))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	// SPA fallback: serve the app shell so the client router can resolve the
	// URL. embed.FS rejects traversal (no "..", no absolute), so this path is
	// also reached for any attempted escape and is therefore safe.
	if data, err = fs.ReadFile(assets, path.Join(assetRoot, indexName)); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	// The shell itself is missing: the embed is broken, not the request.
	http.Error(w, "dashboard not embedded", http.StatusInternalServerError)
}

// serveAPINotFound returns the JSON 404 envelope for any unmatched API route.
func serveAPINotFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(notFoundBody)
}

// contentType maps an asset name to its Content-Type. HTML is handled
// explicitly so the SPA shell always carries a charset; other types fall back
// to content sniffing, which is correct for the future _next/ static assets.
func contentType(name string, data []byte) string {
	switch path.Ext(name) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	}
	return http.DetectContentType(data)
}
