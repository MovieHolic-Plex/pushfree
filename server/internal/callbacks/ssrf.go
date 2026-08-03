// Package callbacks implements the todo-25 callback worker: best-effort
// delivery of an acknowledged receipt's JSON to the sender-configured
// callback_url, with SSRF egress protection and a 1-minute retry policy.
//
// Layering mirrors the receipts domain (scheduler.go / cancel.go): this
// package owns a narrow persistence surface (the Store interface) and the pure
// SSRF predicate; the concrete SQLite implementation lives in
// internal/store/sqlite/callback_worker.go. The worker satisfies the todo-23
// api.AckHook seam structurally (it has OnAcknowledged(ctx, receiptID) error),
// so wiring (cmd/pushfree) connects it without the api package importing
// callbacks.
//
// SSRF model. Egress is allow-by-denylist: a URL is blocked when its host
// resolves (via an injectable Resolver) to ANY address that is loopback, IPv4
// link-local (169.254/16), IPv6 link-local (fe80::/10), RFC1918
// (10/8,172.16/12,192.168/16), IPv6 ULA (fc00::/7), or the unspecified
// address. The callback-allowed-hosts config (Options.AllowedHosts) names hosts
// (by bare hostname OR host:port) that bypass the denylist -- the operator's
// explicit trust list. Two checkpoints use ValidateURL:
//
//   - at enqueue (Enqueue), the initial callback_url is validated; a blocked
//     URL yields ErrSSRFBlocked and no callback row is ever created;
//   - at redirect time, the HTTP client's CheckRedirect hook re-validates each
//     Location target, so a public URL that 3xx-redirects to an internal
//     address is blocked before the dial.
//
// The denylist is expressed with the standard-library netip predicates, which
// unmap IPv4-in-IPv6 addresses, so ::ffff:127.0.0.1 is treated as loopback.
package callbacks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
)

// ErrSSRFBlocked is returned by ValidateURL (and surfaces from Enqueue) when a
// URL targets a blocked address or uses a disallowed scheme. Callers branch on
// errors.Is(err, ErrSSRFBlocked). It is a hard reject: no callback row is
// created and no HTTP request is issued.
var ErrSSRFBlocked = errors.New("callbacks: url blocked by SSRF policy")

// Resolver maps a hostname to its IP addresses. The production default
// (defaultResolver) uses net.DefaultResolver.LookupNetIP, which returns IP
// literals (e.g. "169.254.169.254") without a DNS round-trip. Tests inject a
// fake to assert DNS-rebinding (a public hostname resolving to a private IP).
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// defaultResolver resolves host through the system resolver. It is the
// production default; LookupNetIP returns an IP literal verbatim, so SSRF
// checks on literal-IP URLs (the common metadata-endpoint case) never hit DNS.
func defaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// IsBlockedIP reports whether ip falls into a default-deny range: the
// unspecified address, any loopback, any link-local unicast, or any RFC1918 /
// ULA private address. These are exactly the addresses that must not receive a
// server-initiated callback (cloud metadata endpoints, intranet services, the
// loopback of the server or another tenant, etc.). netip unmaps
// IPv4-in-IPv6, so ::ffff:127.0.0.1 is loopback here.
func IsBlockedIP(ip netip.Addr) bool {
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// hostAllowed reports whether host (bare hostname, no port) or fullHost
// (host:port) is named in the allow-list. Both forms are accepted so an
// operator can trust either "example.com" (any port) or "example.com:8443"
// (one endpoint).
func hostAllowed(host, fullHost string, allowed map[string]bool) bool {
	if len(allowed) == 0 {
		return false
	}
	return allowed[host] || allowed[fullHost]
}

// ValidateURL applies the SSRF policy to a single URL. It returns nil if the
// URL is permitted, ErrSSRFBlocked (wrapped) if it is denied. allowed is the
// operator's trust set (host and/or host:port entries); resolve resolves the
// hostname to IPs. A nil resolve falls back to the system resolver.
//
// Denial cases:
//   - parse failure, empty host, or a scheme other than http/https;
//   - the host is NOT allow-listed AND resolves to at least one blocked IP;
//   - the host is NOT allow-listed AND fails to resolve (fail-closed: an
//     unresolvable host cannot be verified safe).
//
// The allow-list short-circuits resolution: a named host is trusted regardless
// of what it resolves to (the operator owns that decision), so the test server
// on 127.0.0.1 is reachable when named.
func ValidateURL(ctx context.Context, rawURL string, allowed map[string]bool, resolve Resolver) error {
	if resolve == nil {
		resolve = defaultResolver
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: parse %q: %v", ErrSSRFBlocked, rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q not allowed", ErrSSRFBlocked, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrSSRFBlocked)
	}
	if hostAllowed(host, u.Host, allowed) {
		return nil
	}
	ips, err := resolve(ctx, host)
	if err != nil {
		// Fail-closed: cannot verify the host is safe -> block.
		return fmt.Errorf("%w: resolve %q: %v", ErrSSRFBlocked, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: no addresses for %q", ErrSSRFBlocked, host)
	}
	for _, ip := range ips {
		if IsBlockedIP(ip) {
			return fmt.Errorf("%w: %s resolves to blocked %s", ErrSSRFBlocked, host, ip)
		}
	}
	return nil
}
