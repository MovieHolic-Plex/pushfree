# syntax=docker/dockerfile:1.7
#
# Multi-stage build for the pushfree server.
#
#   docker build -t pushfree server/
#
# Stage 1 builds two static, pure-Go (CGO_ENABLED=0) binaries with the golang
# alpine toolchain. Stage 2 copies only those binaries onto a CA-certificate
# enabled distroless/static image (CA certs are required so the server's
# callback worker / FCM channel can verify outbound HTTPS targets), runs as a
# non-root user, and wires a real in-container HEALTHCHECK.
#
# Why a dedicated healthcheck binary? distroless/static ships no shell and no
# curl/wget, and cmd/pushfree has no -healthcheck subcommand (verified: the
# binary's only flag is -config). A tiny compiled helper is therefore the only
# way to run a genuine GET /health probe inside the container. It speaks raw
# HTTP/1.0 over TCP (no net/http, no crypto/tls) to stay small.
#
# Size budget (linux/amd64, -trimpath -ldflags "-s -w"):
#   pushfree binary  ~16.0 MB  (modernc.org/sqlite dominates)
#   pf-healthcheck   ~2.1 MB
#   distroless base  ~3.0 MB   (CA certs + tzdata + nonroot)
#   total           ~21 MB     (1 MB over the <20 MB target; see README note)

# ---------------------------------------------------------------------------
# Stage 1: builder
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS builder

# skippable on alpine, but explicit about the no-cgo single-static-binary rule.
ENV CGO_ENABLED=0

WORKDIR /src

# Module files first for dependency-layer caching.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# The whole module. The embedded dashboard is committed under
# internal/webmount/web/out (see internal/webmount/embed.go), so a plain
# `go build` embeds the real UI with no Node toolchain required. The build
# context excludes host artifacts via server/.dockerignore.
COPY . .

# Target platform (populated by buildx; defaults for plain `docker build`).
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV GOOS=${TARGETOS} GOARCH=${TARGETARCH}

# Main server binary.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/pushfree ./cmd/pushfree

# In-container healthcheck helper. The source is generated in the build stage
# only (it is not a tracked module file), keeping this Dockerfile self-contained.
RUN <<'BUILD'
set -eu
mkdir -p /src/hc
cat > /src/hc/main.go <<'GO'
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	addr := os.Getenv("PUSHFREE_HEALTH_ADDR")
	if addr == "" {
		addr = "127.0.0.1:2586"
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: dial:", err)
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /health HTTP/1.0\r\nHost: localhost\r\n\r\n"); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: write:", err)
		os.Exit(1)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: read:", err)
		os.Exit(1)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "2") {
		fmt.Fprintln(os.Stderr, "healthcheck: unhealthy:", strings.TrimSpace(line))
		os.Exit(1)
	}
}
GO
go build -trimpath -ldflags="-s -w" -o /out/pf-healthcheck /src/hc/main.go
BUILD

# Empty placeholder so the final image can create a non-root-owned data dir
# (distroless has no mkdir); the named volume inherits this ownership on first
# mount, letting the nonroot user write its SQLite database.
RUN mkdir -p /out/data

# ---------------------------------------------------------------------------
# Stage 2: final runtime image
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="pushfree" \
      org.opencontainers.image.description="Self-hosted push notification server (Pushover-compatible)" \
      org.opencontainers.image.source="https://github.com/pushfree/pushfree" \
      org.opencontainers.image.licenses="Apache-2.0"

# Default the SQLite database into the mounted volume; PUSHFREE_* env always
# wins over the config file. PUSHFREE_HEALTH_ADDR is what the helper probes.
ENV PUSHFREE_DB_FILE=/var/lib/pushfree/pushfree.db \
    PUSHFREE_HEALTH_ADDR=127.0.0.1:2586

COPY --from=builder /out/pushfree /usr/local/bin/pushfree
COPY --from=builder /out/pf-healthcheck /usr/local/bin/pf-healthcheck

# Create /var/lib/pushfree owned by the nonroot user (uid 65532). A named Docker
# volume mounted here copies this ownership on first init; without it the volume
# would be root-owned and the server could not write its database (the volume
# permission failure scenario).
COPY --chown=65532:65532 --from=builder /out/data /var/lib/pushfree/

USER nonroot:nonroot

EXPOSE 2586

# GET /health every 30s; the server returns {"status":"ok"} (200) when live.
# NOTE: the probe uses plain HTTP. The server's default/recommended mode is HTTP
# behind a TLS-terminating reverse proxy. If you enable built-in TLS, disable
# this container healthcheck (HEALTHCHECK NONE) and probe over HTTPS externally.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/pf-healthcheck"]

ENTRYPOINT ["/usr/local/bin/pushfree"]
CMD ["-config", "/etc/pushfree/pushfree.toml"]
