# Getting started

This guide takes you from a source checkout to sending and receiving a message
with pushfree. It assumes a Go 1.26+ toolchain.

## 1. Build the server

The server is a single static binary built with the pure-Go SQLite driver, so
there is no cgo and no external dependencies at build time.

```sh
cd server
go build -o pushfree ./cmd/pushfree
```

The binary reads configuration from an optional TOML file (see
[configuration.md](configuration.md)); with no file and no environment
variables it serves HTTP on `:2586` using a local `pushfree.db` SQLite file.

## 2. Start the server

```sh
./pushfree
```

The server logs JSON to stderr. With no TLS configured it logs a warning and
serves plain HTTP — fine for a local trial, not for production (see
[self-hosting.md](self-hosting.md) for TLS / reverse proxy).

Confirm it is up:

```sh
curl -sf http://localhost:2586/health   # {"status":"ok"}
```

## 3. Create your account

Accounts are created over JSON under `/v1/`. **The first account to register
becomes the admin**; subsequent accounts are normal users.

```sh
curl -X POST http://localhost:2586/v1/accounts \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"at-least-8-chars"}'
# -> {"status":1,"user_key":"<30-char key>"}
```

Passwords are hashed with argon2id (RFC 9106: m=64 MiB, t=3, p=4); the
`user_key` is a 30-character `[A-Za-z0-9]` identifier. Source:
`server/internal/api/security.go`.

## 4. Log in to get a session cookie

The dashboard and the management routes (`/v1/apps`, `/v1/accounts/*`) are
authenticated with an HMAC-SHA256-signed session cookie, not with the
`user_key`.

```sh
curl -sc cookies.txt -X POST http://localhost:2586/v1/accounts/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"at-least-8-chars"}'
```

The cookie is `HttpOnly`, `SameSite=Lax`, and marked `Secure` when the request
arrived over TLS. Sessions last 30 days. Source:
`server/internal/api/security.go`.

## 5. Create an app token

Sending messages requires an **app token** (the Pushover "application token").
It is distinct from your `user_key`.

```sh
curl -sb cookies.txt -X POST http://localhost:2586/v1/apps \
  -H 'Content-Type: application/json' \
  -d '{"name":"monitoring"}'
# -> {"status":1,"token":"<30-char token>"}
```

## 6. Send a message

`POST /1/messages.json` is form/multipart-encoded and accepts the Pushover
field set. Auth is the app token (`token`) and the recipient's `user` key.

```sh
curl -X POST http://localhost:2586/1/messages.json \
  -d "token=$TOKEN" -d "user=$USERKEY" \
  -d "message=Server CPU is high" -d "priority=1" -d "sound=siren"
# -> {"status":1,"request":"<uuid>"}
```

For an emergency alert use `priority=2`; the response then also includes a
30-char `receipt` you can poll, acknowledge, or cancel (see
[api.md](api.md) and [API-COMPAT.md](API-COMPAT.md)).

## 7. Open the dashboard

The admin dashboard is embedded in the binary and served at `/admin/`. Open
`http://localhost:2586/admin/` in a browser and log in with your email and
password. From there you can manage app tokens, watch a live message feed,
browse receipts, and configure quiet hours.

## 8. Receive messages on a device

To receive messages you register a device and then connect a realtime
transport. Device registration requires a session (the dashboard's "add
server" flow does this for you):

```sh
curl -sb cookies.txt -X POST http://localhost:2586/1/devices/login.json \
  -H 'Content-Type: application/json' -d '{"name":"my-laptop"}'
# -> {"status":1,"device_id":"<30-char>","secret":"<30-char>"}
```

Only `sha256(secret)` is stored; the plaintext `secret` is returned once. Then
connect a client:

- **Android app** — see [clients.md](clients.md).
- **Desktop app** — see [clients.md](clients.md).
- **Raw WebSocket** — connect to `ws://localhost:2586/1/ws?since=0`, send the
  first line `{"type":"login","device_id":"...","secret":"..."}`, and read
  `message` / `keepalive` frames. See [api.md](api.md#realtime-transports).

## Where to go next

- [configuration.md](configuration.md) — every config key and its default.
- [api.md](api.md) — full endpoint reference.
- [self-hosting.md](self-hosting.md) — TLS, reverse proxy, backups, Postgres.
- [API-COMPAT.md](API-COMPAT.md) — how pushfree maps to Pushover, including
  the deliberate deviations.
