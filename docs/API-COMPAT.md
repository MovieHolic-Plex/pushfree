# Pushover API compatibility matrix

This documents how pushfree maps to the Pushover API, including the deliberate
deviations. Every claim cites the source file that implements it. The
canonical Pushover contract is `EB/A1-pushover-api.md` (research facts block);
pushfree implements against it.

## Endpoint matrix

| Pushover endpoint                             | pushfree | Source file                                              | Notes                                                            |
| --------------------------------------------- | -------- | -------------------------------------------------------- | ---------------------------------------------------------------- |
| `POST /1/messages.json`                       | yes      | `server/internal/api/messages.go`                        | Full field contract (see below).                                 |
| `POST /1/users/validate.json`                 | yes      | `server/internal/api/validate.go`                        | `licenses` always `[]`. Bad token is `400` (Pushover parity).    |
| `GET /1/sounds.json`                          | yes      | `server/internal/api/sounds.go`                          | Fixed 23 built-ins; no custom-sound synthesis.                   |
| `GET /1/apps/limits.json`                     | yes      | `server/internal/api/quota.go`                           | `{count,limit,remaining,reset}`.                                 |
| `POST /1/groups.json` (CRUD)                  | yes      | `server/internal/api/groups.go`                          | `group_key` usable verbatim in `user`.                           |
| `POST /1/subscriptions` (+ migrate)           | yes      | `server/internal/api/subscriptions.go`                   | Codes, per-app dynamic keys, migrate.                            |
| `GET /1/receipts/{receipt}.json`              | yes      | `server/internal/api/receipts.go`                        | A1 snapshot fields, 7-day query window.                          |
| `POST /1/receipts/{receipt}/acknowledge.json` | yes      | `server/internal/api/receipts.go`                        | Device secret **or** app token.                                  |
| `POST /1/receipts/{receipt}/cancel.json`      | yes      | `server/internal/api/cancel.go`                          | Pending-only; terminal returns `409`.                            |
| `POST /1/receipts/cancel_by_tag.json`         | yes*     | `server/internal/api/cancel.go`                          | `*` tag in form body, not path (Go 1.22 mux). See deviations.    |
| Open Client `/1/devices/login.json`           | yes      | `server/internal/hub/http.go`                            | SHA-256(secret) stored.                                          |
| Open Client `GET /1/messages.json?since=`     | yes      | `server/internal/hub/http.go`                            | Pull, <= 100, oldest first.                                      |
| Open Client `GET /1/ws`                       | yes      | `server/internal/hub/ws.go`                              | Login line, open frame, replay, keepalive, close `4001`.         |
| Open Client `GET /1/sse`                      | yes      | `server/internal/hub/sse.go`                             | SSE fallback (Pushover has no SSE; pushfree addition).           |
| UnifiedPush distributor                       | yes      | `server/internal/up/up.go`                               | ntfy-parity `/up/{sub}/*`; not a Pushover feature.               |

## messages.json field semantics

| Field              | Pushover                                  | pushfree                                                                | Source                                     |
| ------------------ | ----------------------------------------- | ----------------------------------------------------------------------- | ------------------------------------------ |
| `token`            | app token                                 | 30-char `[A-Za-z0-9]`; present-but-invalid -> `401`, missing -> `400`   | `server/internal/api/apps.go`              |
| `user`             | user key, <= 50 comma-separated           | same; each key resolves to a user or a group's members                  | `server/internal/api/messages.go`          |
| `message`          | <= 1024 chars                             | <= 1024 UTF-8 **runes** (`utf8.RuneCountInString`)                      | `server/internal/api/messages.go`          |
| `title`            | <= 250                                    | <= 250 runes                                                            | `server/internal/api/messages.go`          |
| `url`              | <= 512                                    | <= 512 runes                                                            | `server/internal/api/messages.go`          |
| `url_title`        | <= 100                                    | <= 100 runes                                                            | `server/internal/api/messages.go`          |
| `priority`         | `-2..2`                                   | same; `2` returns a `receipt`                                           | `server/internal/api/messages.go`          |
| `device`           | comma list, <= 25 chars `[A-Za-z0-9_-]`   | same                                                                    | `server/internal/api/messages.go`          |
| `html`/`monospace` | `1` enables; mutually exclusive           | same; only literal `1` is true                                          | `server/internal/api/messages.go`          |
| `attachment`       | one file, <= 5,242,880 bytes              | same; multipart **or** `attachment_base64`+`attachment_type`            | `server/internal/api/messages.go`          |
| `timestamp`        | Unix seconds                              | accepted; unparseable ignored                                           | `server/internal/api/messages.go`          |
| `ttl`              | seconds, discard undelivered              | same                                                                    | `server/internal/api/messages.go`          |
| `tags`             | priority-2; cancel-by-tag                 | same                                                                    | `server/internal/api/messages.go`          |
| `callback`         | priority-2 webhook on ack                 | same; SSRF allowlist enforced                                           | `server/internal/api/messages.go`          |
| `encrypted`        | `1` E2EE                                  | opaque storage/forward; server never decrypts                           | `server/internal/api/messages.go`, `e2ee/` |
| `sound`            | built-in or custom                        | **accepted as-is** (unknown/custom stored, not synthesized)             | `server/internal/api/messages.go`          |

**Response envelope:** success `{"status":1,"request":"<uuid>"}` and, for
`priority=2`, `"receipt":"<30-char>"`; errors
`{"status":0,"errors":[...],"request":"<uuid>"}`. Source:
`server/internal/api/messages.go`.

## Receipt lifecycle (priority-2)

| Aspect                | Pushover                                          | pushfree                                                      | Source                                     |
| --------------------- | ------------------------------------------------- | ------------------------------------------------------------- | ------------------------------------------ |
| receipt id            | 30-char `[A-Za-z0-9]`                             | same (shared generator with `user_key`)                       | `server/internal/api/security.go`          |
| states                | pending/delivered/...                             | `pending`, `delivered`, `acknowledged`, `expired`, `canceled` | `server/internal/receipts/statemachine.go` |
| retry interval        | >= 30 s                                           | floor 30 s (`MinRetryInterval`)                               | `server/internal/receipts/scheduler.go`    |
| expire window         | <= 10800 s (3 h)                                  | ceiling 10800 s (`MaxExpire`)                                 | `server/internal/receipts/scheduler.go`    |
| attempt cap           | 50                                                | 50 (`MaxAttempts`); cap checked before timeout                | `server/internal/receipts/scheduler.go`    |
| query window          | ~1 week                                           | 7 days, then GC                                               | `server/internal/api/receipts.go`          |
| ack                   | device secret                                     | device secret **or** app token (idempotent)                   | `server/internal/api/receipts.go`          |
| callback              | POST receipt JSON on ack, retry ~1 min on non-2xx | 60 s retry, SSRF allowlist                                    | `server/internal/callbacks/worker.go`      |
| durable timers        | (Pushover-internal)                               | persisted timers, atomic claim, crash recovery, jitter +-10%  | `server/internal/receipts/` (todo 22)      |

### M6 — WebSocket-only retry semantics

> WS-only recipients — those with no FCM/UnifiedPush token, reachable only
> over a live WS/SSE connection — receive emergency retries via **since-cursor
> replay on reconnect**, not an active push. The retry still fires and is
> counted (so the 50-cap and expire accounting are identical across transport
> mixes), but for a WS-only recipient the redelivery is a retention no-op: the
> message row stays in the recipient's `since` cursor so the next reconnect
> replays it. This mirrors Pushover, where an unconnected client picks up the
> emergency alert on its next connect.
>
> Source: the `Redeliver` doc comment in `server/internal/receipts/scheduler.go`.

## Quota and rate-limit headers

| Header / value          | Pushover                          | pushfree                                                 | Source                                  |
| ----------------------- | --------------------------------- | -------------------------------------------------------- | --------------------------------------- |
| monthly limit           | 10,000 / app                      | 10,000 / **user**, shared across that user's apps        | `server/internal/api/applimit.go`       |
| reset zone              | America/Chicago (Central Time)    | same (`quota.CentralTime`), DST-aware                    | `server/internal/quota/quota.go`        |
| `429` body              | `{"status":0,"errors":[...]}`     | same; pre-write gate, `X-Limit-App-Remaining: 0`         | `server/internal/api/quota.go`          |
| `X-Limit-App-Limit`     | yes                               | yes                                                      | `server/internal/api/applimit.go`       |
| `X-Limit-App-Remaining` | yes                               | yes (live counter)                                       | `server/internal/api/applimit.go`       |
| `X-Limit-App-Reset`     | yes                               | yes (epoch seconds, next CT month boundary)              | `server/internal/api/applimit.go`       |
| retries counted?        | no                                | no (delivery retries never re-count; todo 26 regression) | `server/internal/api/messages.go`       |
| group cost              | per member                        | per concrete recipient (`len(resolved IDs)`)             | `server/internal/api/messages.go`       |

## Subscription keys

| Aspect                  | Pushover                       | pushfree                                                  | Source                                  |
| ----------------------- | ------------------------------ | --------------------------------------------------------- | --------------------------------------- |
| `subscription_code`     | code; web redirect to approve  | 30-char code; `/subscribe/<code>` page (dashboard)        | `server/internal/api/subscriptions.go`  |
| `subscribed_user_key`   | dynamic per app                | per **app+user** (stable on re-approve)                   | `server/internal/api/subscriptions.go`  |
| `migrate.json`          | re-parent + remap keys         | same; old keys invalidated, new minted                    | `server/internal/api/subscriptions.go`  |
| resolves like `user`?   | yes                            | yes (single `ResolveRecipients` path)                     | `server/internal/store/`                |

## Quiet hours

| Aspect            | Pushover                          | pushfree                                                | Source                                  |
| ----------------- | --------------------------------- | ------------------------------------------------------- | --------------------------------------- |
| storage           | per-user window + tz              | `quiet_start`/`quiet_end` (`HH:MM`) + IANA `tz`         | `server/internal/api/accounts.go`       |
| hold behaviour    | priority-2/-1/0 held              | `priority <= 0` held; `priority >= 1` bypasses          | `server/internal/quiethours/hold.go`    |
| flush             | on window end                     | flushed on window end (since-replay delivers naturally) | `server/internal/quiethours/manager.go` |
| per-recipient     | yes                               | yes (evaluated per recipient)                           | `server/internal/quiethours/`           |

## Explicit deviations

These are **deliberate** differences from Pushover. They are surfaced here
rather than hidden.

1. **Message retention is 30 days, not 21.** `messages-retention` default
   `720h`. Source: `server/internal/config/config.go`,
   `server/internal/retention/sweeper.go`.
2. **No "at most 2 concurrent connections" limit.** Pushover caps simultaneous
   Open Client connections per device; pushfree (self-hosted, single-node)
   does not enforce this. Source: `server/internal/hub/hub.go` (no such cap).
3. **WS-only retry is a replay, not a push (M6).** See the M6 note above.
4. **`cancel_by_tag` takes the tag in the form body, not the path.** Go 1.22's
   `ServeMux` does not permit a `.json` suffix on a wildcard segment, and a
   bare `{tag}` would collide with `{receipt}/cancel.json`. The endpoint is
   `POST /1/receipts/cancel_by_tag.json` with `tag` in the form. Source:
   `server/internal/api/cancel.go`.
5. **`ack` accepts an app token in addition to a device secret.** Pushover's
   contract acks via the device; pushfree also allows the owning app token.
   Source: `server/internal/api/receipts.go`.
6. **Sounds are not synthesized.** Unknown/custom `sound` values are accepted
   and stored as-is (Pushover falls back to its default). `sounds.json` lists
   only the 23 built-ins. Source: `server/internal/api/sounds.go`.
7. **`validate.json` returns `400` (not `401`) for a bad token.** This matches
   Pushover's own behaviour for these metadata endpoints. Source:
   `server/internal/api/validate.go`.
8. **Quota is per-user, not per-app.** All of a user's apps share one monthly
   counter. Source: `server/internal/api/applimit.go`.
9. **SSE is an addition.** Pushover's Open Client is WS-only; pushfree exposes
   `GET /1/sse` as a fallback. Source: `server/internal/hub/sse.go`.
10. **No server-side message decryption.** `encrypted=1` fields are stored and
    forwarded opaquely; only clients decrypt. Source: `server/internal/e2ee/`.

## Not implemented (out of scope by design)

These Pushover features are deliberately absent (see the plan's "Must NOT
have"): glances, licensing/teams administration, email/SMS/voice gateways,
inbound webhooks, XML variants, Open Client byte-for-byte compatibility, iOS
client, browser push (VAPID/PWA), and a hosted cloud offering. There is no
ntfy-style native publish API (`PUT /topic`).
