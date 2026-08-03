# Security policy

This document covers how to report security vulnerabilities in PushFree and
summarizes the security-relevant design of the server. It is not a complete
threat model; it points at the source for each claim.

## Reporting a vulnerability

If you find a security issue, **do not open a public GitHub issue**. Instead,
report it privately so a fix can be coordinated before disclosure.

- Preferred: use GitHub's "Report a vulnerability" flow on the repository's
  Security tab (private fork advisory).
- If that is unavailable, open a private contact with the maintainers and
  include: a description, reproduction steps, affected version/commit, and any
  proof of concept.

Please give maintainers a reasonable window to investigate and ship a fix
before any public disclosure. We will credit reporters unless they prefer
otherwise.

## Supported versions

Only the latest release line and the tip of `master` receive security fixes.
When a fix is released it is noted in the release notes and, where relevant,
the affected config keys or endpoints are called out here.

## Security posture by area

### Authentication and sessions

- Passwords are hashed with **argon2id** (RFC 9106); the `user_key` is a
  30-char `[A-Za-z0-9]` identifier. Source: `server/internal/api/security.go`.
- Management routes (`/v1/*`) authenticate with an HMAC-SHA256-signed session
  cookie (`HttpOnly`, `SameSite=Lax`, `Secure` under TLS, 30-day lifetime). A
  random per-process signing secret is used if `auth-secret` is unset — set a
  stable high-entropy `auth-secret` (or `PUSHFREE_AUTH_SECRET`) in production.
  Source: `server/internal/api/security.go`, `server/cmd/pushfree/main.go`.
- The first account to register becomes the admin. Source:
  `server/internal/api/accounts.go`.

### App tokens and the Pushover send path

- `POST /1/messages.json` and the rest of `/1/*` authenticate with an app
  `token` (30-char). A present-but-invalid token returns `401`; a malformed or
  missing token returns `400`. Source: `server/internal/api/apps.go`,
  `server/internal/api/messages.go`.
- Input bounds are enforced in UTF-8 runes (message <= 1024, title <= 250,
  url <= 512, url_title <= 100, user comma-list <= 50). A single attachment is
  capped at 5 MiB. Source: `server/internal/api/messages.go`.
- Per-user monthly send quota (default 10,000) is gated **pre-write** with a
  `429`; the limit headers `X-Limit-App-*` are returned on `/1/*` responses.
  Source: `server/internal/api/quota.go`, `server/internal/quota/quota.go`.

### End-to-end encryption (E2EE)

PushFree implements the Pushover per-field E2EE format for `encrypted=1`
messages. The wire format is, per field:

```text
GZIP(plaintext)
  -> AES-256-CBC (random 16-byte IV, PKCS7 padding)
  -> HMAC-SHA256(key, IV || ciphertext)
  -> base64( IV || ciphertext || hmac )
```

- The 256-bit key is a 64-character hex string, provisioned **out-of-band**;
  **the server never receives or stores the key.**
- The server stores and transports `encrypted=1` fields **opaquely** — it
  assigns the request value verbatim into the stored row and never decrypts.
  Only metadata stays plaintext. Source: `server/internal/api/messages.go`.
- The decrypt path in `server/internal/e2ee/e2ee.go` is the **client-side
  reference implementation** (consumed by the Android and desktop clients), not
  a server inbound path. It verifies the HMAC before any CBC/PKCS7 work
  (encrypt-then-MAC) in constant time, and collapses all post-MAC failures
  (padding, gzip, short blob) into a single generic error to avoid a padding
  oracle on the legacy CBC construction.

The shared test vectors live at
`server/internal/e2ee/testdata/e2ee_vectors.json` and are consumed by the Go,
Android, and desktop suites, so all three platforms must agree on the format.
See [docs/API-COMPAT.md](docs/API-COMPAT.md) for the field matrix.

### Receipt callback egress (SSRF protection)

Receipt callbacks (priority-2 ack webhooks) are delivered by a worker that
applies an **allow-by-denylist** SSRF policy before any outbound request:

- Blocked by default: loopback, link-local (`169.254/16`, `fe80::/10`),
  RFC1918 (`10/8`, `172.16/12`, `192.168/16`), IPv6 ULA (`fc00::/7`), and the
  unspecified address. This blocks cloud metadata endpoints (e.g.
  `169.254.169.254`) and intranet services by default. `netip` unmaps
  IPv4-in-IPv6, so `::ffff:127.0.0.1` is treated as loopback.
- Only `http`/`https` schemes are permitted.
- An operator trust-list (`callback-allowed-hosts` / `PUSHFREE_CALLBACK_ALLOWED_HOSTS`,
  entries as bare host or `host:port`) bypasses the denylist for named hosts.
- The policy is checked at enqueue **and** on every redirect target
  (the HTTP client's `CheckRedirect` re-validates each `Location`), so a public
  URL that 3xx-redirects to an internal address is blocked before the dial.
- A host that fails to resolve is **fail-closed** (blocked), since it cannot be
  verified safe.

Source: `server/internal/callbacks/ssrf.go`, `server/internal/callbacks/worker.go`.
Configuration: [docs/configuration.md](docs/configuration.md).

If you operate PushFree, only add hosts to `callback-allowed-hosts` that you
control and trust — those entries are reachable regardless of what they resolve
to.

### Rate limiting and resource bounds

- The durable emergency-retry scheduler is capped at 50 attempts per receipt,
  with a 30-second floor and a 3-hour expiry ceiling, so a single priority-2
  message cannot run away indefinitely. Source:
  `server/internal/receipts/scheduler.go`, `server/internal/timers/`.
- Message retention is bounded (default 30 days), undownloaded attachment
  BLOBs expire (default 3 days), and receipts are garbage-collected after a
  7-day query window. Source: `server/internal/retention/`.

### TLS

Serve behind a TLS-terminating reverse proxy (recommended), or enable built-in
TLS by setting **both** `tls-cert-file` and `tls-key-file`. Setting only one is
a startup error. `GET /metrics` is **not** authenticated — bind the listener to
a private address or scrape over the loopback only. Source:
`server/internal/config/config.go`, [docs/self-hosting.md](docs/self-hosting.md).

## No credentials in the repository

- Never commit real secrets, passwords, private keys, service-account JSONs,
  keystore files, or production configuration.
- The root `.gitignore` excludes `.omo/`, keystores (`*.keystore`, `*.jks`),
  `local.properties`, build outputs, and `node_modules/`. FCM service-account
  JSON is referenced by path (`fcm-credentials-file`) and never committed.
- Example/sample files (`server/pushfree.example.toml`,
  `deploy/pushfree.example.toml`) contain only defaults and placeholders.
- If you accidentally commit a secret, treat it as compromised: rotate it
  immediately and ask a maintainer to remove the sensitive data from history.

## Disclosure policy

- We acknowledge reports promptly and work with the reporter on a fix and a
  coordinated disclosure timeline.
- A CVE may be requested for confirmed, independently exploitable issues.
- Security fixes are released as patch versions and announced in the release
  notes.
