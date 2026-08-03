package webmount

import (
	"encoding/json"
	"io/fs"
	"mime"
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
	// staticPrefix is the Next.js content-addressed asset subtree. Files under
	// it are immutable (their names carry a content hash) and may be cached
	// aggressively by clients and intermediaries.
	staticPrefix = "_next/static/"
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

// mimeByExt maps web asset extensions to their Content-Type. It is consulted
// before sniffing so that JavaScript and CSS are served with executable MIME
// types. http.DetectContentType classifies both as "text/plain", which makes
// browsers refuse to execute them under strict MIME checking and would silently
// break the SPA. The map is deliberately OS-independent: Windows has no
// /etc/mime.types, so mime.TypeByExtension alone is unreliable here.
var mimeByExt = map[string]string{
	".html": "text/html; charset=utf-8",
	".htm":  "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".mjs":  "text/javascript; charset=utf-8",
	".cjs":  "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".map":  "application/json; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".ico":  "image/x-icon",
	".woff": "font/woff",
	".woff2": "font/woff2",
	".ttf":  "font/ttf",
	".otf":  "font/otf",
	".eot":  "application/vnd.ms-fontobject",
	".wasm": "application/wasm",
	".xml":  "application/xml; charset=utf-8",
}

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
//	  GET /admin/*  embedded static asset. Extensionless paths use clean-URL
//	                resolution (/dashboard -> dashboard.html, /foo/bar ->
//	                foo/bar.html, then foo/bar/index.html); any extensionless
//	                path that still misses falls back to index.html with 200 so
//	                client-side SPA routing works. A path with an extension
//	                that does not resolve to a real asset returns 404 (never
//	                the HTML shell, so a missing .js is not mis-served).
//	  /api/*        JSON 404 envelope (any method), never SPA-fallback.
//
// More specific routes registered elsewhere (e.g. the real API under /1/ and
// /v1/, or explicit /api/... handlers) outrank the /api/ subtree by ServeMux
// specificity, so this never shadows them.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+adminPrefix, serveAdmin)
	mux.HandleFunc("/api/", serveAPINotFound)
}

// serveAdmin serves an embedded asset, applying clean-URL resolution and SPA
// fallback as described in Register.
func serveAdmin(w http.ResponseWriter, r *http.Request) {
	// net/http already canonicalises r.URL.Path (collapsing "//", resolving
	// "."/".."), and embed.FS independently rejects ".." and absolute paths,
	// so the trimmed name is safe to use as an asset key.
	name := strings.TrimPrefix(r.URL.Path, adminPrefix)

	if rel, data, ok := resolveAsset(name); ok {
		writeAsset(w, rel, data)
		return
	}

	// SPA fallback: only for extensionless routes (client-side paths). A
	// missing real asset (.js/.css/...) must 404 rather than return HTML with
	// an HTML content-type, which would be misleading and break the browser.
	if path.Ext(name) == "" {
		if data, err := fs.ReadFile(assets, path.Join(assetRoot, indexName)); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		// The shell itself is missing: the embed is broken, not the request.
		http.Error(w, "dashboard not embedded", http.StatusInternalServerError)
		return
	}

	http.NotFound(w, r)
}

// resolveAsset maps a request name to an embedded file. It returns the path
// relative to assetRoot that actually matched (so the caller can derive the
// correct Content-Type and cache policy), the file bytes, and whether any
// candidate matched. Clean-URL candidates are only tried for extensionless
// names so a request for a real file with an extension is not silently
// reinterpreted as a route.
func resolveAsset(name string) (rel string, data []byte, ok bool) {
	if name == "" {
		name = indexName
	}
	candidates := []string{name}
	if path.Ext(name) == "" {
		candidates = append(candidates, name+".html", path.Join(name, indexName))
	}
	for _, c := range candidates {
		if b, err := fs.ReadFile(assets, path.Join(assetRoot, c)); err == nil {
			return c, b, true
		}
	}
	return "", nil, false
}

// writeAsset emits a resolved asset with the correct Content-Type and a
// cache policy keyed to whether the asset is content-addressed.
func writeAsset(w http.ResponseWriter, rel string, data []byte) {
	w.Header().Set("Content-Type", contentType(rel, data))
	w.Header().Set("Cache-Control", cacheControl(rel))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// contentType returns the Content-Type for an asset. The extension map is
// authoritative for web types; the OS mime registry is a secondary source;
// content sniffing is the last resort for anything exotic.
func contentType(name string, data []byte) string {
	ext := strings.ToLower(path.Ext(name))
	if ct := mimeByExt[ext]; ct != "" {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return http.DetectContentType(data)
}

// cacheControl returns the cache policy for an asset. Hashed assets under
// _next/static/ are immutable; everything else (notably HTML, whose script
// references change each deploy) is revalidated every request.
func cacheControl(rel string) string {
	if strings.HasPrefix(rel, staticPrefix) {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// serveAPINotFound returns the JSON 404 envelope for any unmatched API route.
func serveAPINotFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(notFoundBody)
}
