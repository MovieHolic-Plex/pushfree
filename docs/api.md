# HTTP API reference

This documents every endpoint the server actually implements. Routes are
registered in `server/internal/server/server.go`, `server/internal/api/accounts.go`
(`Accounts.Register`), `server/internal/api/cancel.go` (`CancelAPI.Register`),
`server/internal/hub/http.go` (`Hub.Routes`, mounted by `Server.MountRealtime`),
and `server/internal/up/up.go` (`Handler.Register`).

Two authentication models are in use:

- **Session cookie** — HMAC-SHA256-signed cookie named `pushfree_session`,
  obtained from `POST /v1/accounts/login`. Used by the dashboard and the
  `/v1/*` management routes. Source: `server/internal/api/security.go`.
- **App token** — a 30-char `[A-Za-z0-9]` string (`token`), used by the
  Pushover-compatible `/1/*` surface and the realtime device routes. App
  tokens are managed via `POST/GET/DELETE /v1/apps`.

All `/1/messages.json` and receipt responses include a per-request `request`
UUID. Error envelopes follow Pushover: `{"status":0,"errors":["..."]}` (and,
on `/1/*` routes, a `request` field). Source: `server/internal/api/messages.go`.

## Health, metrics, dashboard

### `GET /health`

Liveness probe. Body is exactly `{"status":"ok"}` (no trailing newline).
Source: `server/internal/server/server.go`.

### `GET /metrics`

Prometheus text exposition. Exposes `pushfree_messages_received_total`,
`pushfree_messages_delivered_total`, `pushfree_ws_connections`,
`pushfree_receipts_active`, `pushfree_callback_queue_depth`,
`pushfree_quota_remaining`, plus the default `go_*` / `process_*` collectors.
Not authenticated — bind the listener to a private address in production.
Source: `server/internal/metrics/metrics.go`.

### `GET /admin/` (and sub-paths)

The embedded Next.js dashboard. Non-API unknown paths fall back to the SPA
`index.html`; unknown `/api/...` paths return the JSON 404 envelope
`{"status":0,"errors":["not found"]}`. Static `_next/static/` assets are served
with immutable cache headers. Source: `server/internal/webmount/`.

## Accounts and sessions (`/v1/`)

### `POST /v1/accounts`

Register a new account. JSON body: `{ "email", "password" }`. Password must be
>= 8 characters. Returns `201 {"status":1,"user_key":"<30-char>"}`. The first
account to register becomes `admin`; races are resolved safely in a
transaction. A duplicate email is `409`. Source:
`server/internal/api/accounts.go`.

### `POST /v1/accounts/login`

Log in. JSON body: `{ "email", "password" }`. On success sets the
`pushfree_session` cookie and returns `200 {"status":1}`. An unknown email and
a wrong password return the same `401 {"status":0,"errors":["invalid email or
password"]}` to avoid enumeration. Source: `server/internal/api/accounts.go`.

### `GET /v1/accounts/me`

Requires a session. Returns `200 {"status":1,"email","role","user_key",
"quiet_hours":{"start","end","tz"}}`. Source: `server/internal/api/accounts.go`.

### `PUT /v1/accounts/quiet-hours`

Requires a session. JSON body: `{ "quiet_start", "quiet_end", "tz" }`.
`quiet_start`/`quiet_end` are `HH:MM` (or both empty to clear); `tz` is an
IANA timezone. Returns `200 {"status":1,"quiet_hours":{...}}`. Quiet hours are
enforced server-side: messages with `priority <= 0` are held during the
window; `priority >= 1` bypasses. Source: `server/internal/api/accounts.go`,
`server/internal/quiethours/`.

## App tokens (`/v1/apps`)

All require a session.

### `POST /v1/apps`

JSON body `{ "name" }`. Returns `201 {"status":1,"token":"<30-char>"}`.

### `GET /v1/apps`

Returns `200 {"status":1,"apps":[{"id","token","name"}]}`.

### `DELETE /v1/apps/{token}`

Revokes the token. Only the owner may revoke; a foreign token is `404`. A
revoked token fails authentication on `/1/*` (`401`). Source:
`server/internal/api/apps.go`.

## Send (`/1/messages.json`)

### `POST /1/messages.json`

The Pushover-compatible send endpoint. Accepts `application/x-www-form-
urlencoded` or `multipart/form-data` (for a file attachment).

**Required fields:** `token` (app token), `user` (recipient key, comma-
separated list of up to 50 keys), `message`.

**Optional fields and limits** (enforced in `server/internal/api/messages.go`):

| Field                 | Limit / rule                                                     |
| --------------------- | ---------------------------------------------------------------- |
| `message`             | <= 1024 UTF-8 **runes** (not bytes)                              |
| `title`               | <= 250 runes                                                     |
| `url`                 | <= 512 runes                                                     |
| `url_title`           | <= 100 runes                                                     |
| `priority`            | integer `-2..2`                                                  |
| `device`              | comma-separated names, each <= 25 chars `[A-Za-z0-9_-]`          |
| `sound`               | any value; unknown/custom values are stored as-is (see sounds)   |
| `html`                | `1` to enable; mutually exclusive with `monospace`               |
| `monospace`           | `1` to enable; mutually exclusive with `html`                    |
| `timestamp`           | Unix seconds; unparseable values are ignored                     |
| `ttl`                 | seconds, >= 0; undelivered messages past this are discarded      |
| `attachment`          | one file, <= 5,242,880 bytes (5 MiB), multipart OR base64+type   |
| `attachment_base64`   | base64-encoded attachment (with `attachment_type`)               |
| `tags`                | priority-2 only; used by cancel-by-tag                           |
| `callback`            | priority-2 webhook URL (subject to `callback-allowed-hosts`)     |
| `encrypted`           | `1` for end-to-end-encrypted fields (opaque storage)             |

Only the literal `1` is truthy for `html`/`monospace`/`encrypted` (everything
else, including `true`, is false), matching Pushover.

**Auth:** a present-but-invalid/revoked token is `401`. A missing token is
collected with the other field errors as a `400`.

**Recipient resolution:** each key in `user` resolves to one user or to a
group's members (indistinguishable at send time); an unresolved key is `404`
(after validation passes, so it is distinct from a `400`).

**Quota:** the send is charged one unit per concrete recipient. If it would
exceed the monthly limit the server returns `429 {"status":0,"errors":
["application reached monthly message limit"]}` with
`X-Limit-App-Remaining: 0` **before** persisting.

**Success response:** `200 {"status":1,"request":"<uuid>"}`; for
`priority=2` it additionally includes `"receipt":"<30-char>"`.

**Headers:** every `/1/*` response carries `X-Limit-App-Limit`,
`X-Limit-App-Remaining`, `X-Limit-App-Reset` (epoch seconds of the next
America/Chicago month boundary). Source: `server/internal/api/applimit.go`.

## Companion metadata endpoints

### `POST /1/users/validate.json`

Form body `token`, `user`. Confirms the user key belongs to the token's owner
and returns `200 {"status":1,"devices":["name",...],"licenses":[]}`.
`licenses` is always `[]` (pushfree is self-hosted). An invalid token is
`400` (Pushover returns 400 here, not 401). Source:
`server/internal/api/validate.go`.

### `GET /1/sounds.json?token=`

Returns the fixed 23-entry built-in sound catalog:
`200 {"status":1,"sounds":{...}}`. Unknown/custom sound names are accepted
on send (stored as-is); this endpoint lists only the built-ins. Source:
`server/internal/api/sounds.go`.

### `GET /1/apps/limits.json?token=`

Returns the owner's monthly quota usage:
`200 {"count":N,"limit":10000,"remaining":M,"reset":E}`. `reset` is the epoch
second of the next America/Chicago month boundary. An invalid token is `400`.
Source: `server/internal/api/quota.go`.

## Groups (`/1/groups.json`)

All require a session. A group's `group_key` is a 30-char key usable verbatim
in the `user` field of `messages.json`. `name` and `memo` are each <= 200 runes.
Source: `server/internal/api/groups.go`.

| Method | Path               | Body                                                    | Response                                                |
| ------ | ------------------ | ------------------------------------------------------- | ------------------------------------------------------- |
| POST   | `/1/groups.json`   | `{ "name", "memo"?, "members"? }` (comma-sep keys)      | `{"status":1,"group_key":"<30-char>"}`                  |
| GET    | `/1/groups.json`   | -                                                       | `{"status":1,"groups":[{group_key,name,memo,members}]}` |
| PUT    | `/1/groups.json`   | `{ "group_key", "name"?, "memo"?, "add"?, "remove"? }`  | `{"status":1}`                                          |
| DELETE | `/1/groups.json`   | `{ "group_key" }`                                       | `{"status":1}`                                          |

A foreign or absent group is `404` (no cross-user enumeration).

## Subscriptions (`/1/subscriptions`)

Subscription codes with dynamic per-app keys. A `subscribed_user_key` resolves
like a `user_key` in the send path. Source:
`server/internal/api/subscriptions.go`.

### `POST /1/subscriptions`

JSON body `{ "token", "title" }` (`title` <= 250 runes). Token-authenticated.
Returns `{"status":1,"subscription_code":"<30-char>","subscribe_url":"/subscribe/<code>"}`.

### `POST /1/subscriptions/authorize`

Session-authenticated. JSON body `{ "subscription_code" }`. Mints (or returns)
the per-app+user dynamic key. Returns `{"status":1,"subscribed_user_key":"<30-char>"}`.

### `POST /1/subscriptions/migrate.json`

JSON body `{ "subscription_code", "from_app_token", "to_app_token" }`. Both
apps must belong to the same owner and the subscription must currently be
parented on `from_app`. Re-parents the channel and remaps every subscriber key
(old keys invalidated, new keys minted). Returns `{"status":1,"migrated":N}`.

## Receipts (`/1/receipts/...`)

Priority-2 (emergency) sends return a `receipt`. Its lifecycle (pending ->
delivered -> acknowledged / expired / canceled) is driven by the retry
scheduler and durable timers. Source: `server/internal/api/receipts.go`,
`server/internal/receipts/`.

### `GET /1/receipts/{receipt}.json?token=`

Polls the receipt snapshot. Auth: the token must own the receipt's send; a
foreign/unknown receipt is `404`. Returns:

```json
{
  "status": 1,
  "request": "<uuid>",
  "acknowledged": 0,
  "acknowledged_at": null,
  "acknowledged_by": null,
  "acknowledged_by_device": null,
  "delivered": 0,
  "delivered_at": null,
  "expired": 0,
  "expires_at": null,
  "called_back": 0,
  "called_back_at": null
}
```

Boolean fields use `0`/`1`; absent timestamps/keys are `null`. Receipts are
queryable for 7 days, then garbage-collected. Source:
`server/internal/api/receipts.go`.

### `POST /1/receipts/{receipt}/acknowledge.json`

Acknowledges a receipt. Auth accepts EITHER the recipient's device
(`device_id` + `secret`, SHA-256-verified) OR the owning app `token`. On
success the receipt transitions to `acknowledged`, retries stop, and the
callback worker is notified (best-effort). Ack is idempotent. Returns the
post-ack snapshot. Source: `server/internal/api/receipts.go`.

### `POST /1/receipts/{receipt}/cancel.json`

Form body `token`. Cancels a still-pending receipt (terminal-state receipts
return `409 {"status":0,"errors":["receipt is <state> and cannot be
canceled"]}`). Returns `{"status":1,"request":"<uuid>"}`. Source:
`server/internal/api/cancel.go`.

### `POST /1/receipts/cancel_by_tag.json`

Form body `token`, `tag`. Cancels every pending receipt with that tag owned by
the app. Returns `{"status":1,"request":"<uuid>","canceled":["<id>",...]}`. A
tag matching nothing is `200` with an empty list. Source:
`server/internal/api/cancel.go`.

> **URL note:** `cancel_by_tag` takes the tag in the form body, not the path
> (`/cancel_by_tag/{tag}.json`), because Go 1.22's `ServeMux` does not allow a
> suffix on a wildcard segment. See [API-COMPAT.md](API-COMPAT.md).

## Devices and realtime transports

### `POST /1/devices/login.json`

Requires a session. JSON or form body `{ "name", "os"?, "model"? }`. `name`
defaults to `"device"`, <= 25 chars `[A-Za-z0-9_-]`. Returns
`{"status":1,"device_id":"<30-char>","secret":"<30-char>"}`. Only
`sha256(secret)` is stored; the plaintext is returned once. Source:
`server/internal/hub/http.go`.

### `GET /1/messages.json?device_id=&secret=&since=`

Pull endpoint. Authenticates the device, returns a JSON array of that user's
messages with `id > since`, limited to 100, oldest first. `since=0`/absent
returns the latest page. Source: `server/internal/hub/http.go`.

### Realtime transports

| Transport | URL                                            | Auth                                               |
| --------- | ---------------------------------------------- | -------------------------------------------------- |
| WebSocket | `GET /1/ws?since=`                             | first line `{"type":"login","device_id","secret"}` |
| SSE       | `GET /1/sse?device_id=&secret=&since=`         | query string                                       |

**WebSocket protocol** (`server/internal/hub/ws.go`):

1. Client sends one login line; the server validates `device_id`+`secret`. Any
   auth failure closes the connection with application code **`4001`**.
2. Server replies `{"type":"open","last_message_id":<id>}` (its high-water
   mark).
3. Server replays stored messages with `id > since` (oldest first), then
   streams live `message` frames. Frames overlapping the replay page are
   de-duplicated.
4. Server sends `{"type":"keepalive"}` every 45 s (the configured
   `keepalive-interval`). A client read timeout of ~77 s detects a dead
   server (keepalive interval + headroom).

**SSE protocol** (`server/internal/hub/sse.go`): same replay/live semantics,
auth via query string. Events are `event: open` then `event: message`; a
`: keepalive` comment is written every interval.

Delivery is recorded (`messages.delivered_at`, receipt `last_delivered_at`)
when the transport accepts the write. Source: `server/internal/hub/hub.go`.

## UnifiedPush distributor

ntfy-parity URLs so a UnifiedPush connector can register, poll, and ack without
speaking the Pushover wire format. The path `{sub}` is a 4-char
`[A-Za-z0-9]` key derived deterministically from the `device_id`
(HMAC-SHA256 keyed by the server's auth secret). Source:
`server/internal/up/up.go`.

| Method | Path                                | Notes                                                            |
| ------ | ----------------------------------- | ---------------------------------------------------------------- |
| POST   | `/up/{sub}/subscribe.json`          | session-auth; returns `{device_id,secret,sub}` (the derived sub) |
| GET    | `/up/{sub}/messages.json`           | `?device_id=&secret=&since=`; JSON array, <= 100, oldest first   |
| POST   | `/up/{sub}/ack/{msg}`               | idempotent no-op `{"status":1}`                                  |

`messages.json`/`ack` authenticate with `device_id`+`secret` and additionally
verify `DeriveSub(device_id)` matches the path `{sub}`. A mismatch is `404`.

## Field/identifier formats

All secret identifiers are 30-char `[A-Za-z0-9]` from `crypto/rand` with
rejection sampling (unbiased): `user_key`, app `token`, `group_key`,
`subscription_code`, `subscribed_user_key`, receipt id, `device_id`, `secret`.
Source: `server/internal/api/security.go`, `server/internal/hub/auth.go`.
