// Package webmount serves the embedded admin dashboard (a Next.js static
// export) and defines a JSON 404 envelope for unknown API routes.
//
// This is a package-only mount: the live server wires it by calling Register
// on its *http.ServeMux (a separate wiring micro-task). Keeping the mount in
// its own package makes every behaviour provable with httptest, independent
// of server construction.
//
// # Embed location
//
// The Go module lives at server/, so the repo-root web/ (the Next.js export
// target produced by todos 40 and 42) is outside the module and cannot be
// referenced by go:embed (patterns may not contain ".."). The embedded
// assets therefore live in this package under web/out as a server-local copy.
// Todo 42 refreshes that copy from the real build (run from the repo root):
//
//	cd web && pnpm install --frozen-lockfile && pnpm build &&
//	cp -r web/out server/internal/webmount/web/out
//
// The copy is committed (the root .gitignore's "out/" rule is overridden via
// `git add -f` on this path) so a plain `go build` embeds the real dashboard
// with no Node toolchain required at build time.
package webmount

import "embed"

// assets is the embedded static export rooted at web/out (relative to this
// package). The all: prefix includes dotfiles, matching the Next.js export
// layout which may emit _next/ and similar entries later.
//
//go:embed all:web/out
var assets embed.FS
