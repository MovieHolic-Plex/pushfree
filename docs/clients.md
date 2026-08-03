# Clients

pushfree has three first-party clients plus the embedded admin dashboard. All
clients talk to the server over the Open Client protocol: device registration,
realtime transport, and `since`-cursor replay.

## Android app

Source: `android/`. Package id `net.pushfree.android`, `minSdk 26`,
`compileSdk`/`targetSdk 35`, Kotlin + Jetpack Compose.

### Transports (flavors)

The Android app supports **three** delivery transports, selectable per server
subscription. A foreground service keeps the active transport alive with a
persistent notification (the ntfy pattern).

| Flavor          | Source files                                       | Notes                                                                                                                                                                                                                          |
| --------------- | -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **WebSocket**   | `WsTransport` (`.../transport/ws`)                 | Always available. OkHttp WebSocket, 45 s server keepalive vs 77 s read timeout + 30 s ping, Full-Jitter reconnect (base 1 s, cap 60 s), `since` cursor persisted and replayed on reconnect.                                    |
| **FCM**         | `FcmTransport` (`.../transport/fcm`)               | Optional. `google-services.json` is a build-time opt-in: if absent, the transport is disabled at runtime but the build still succeeds. Data messages become `MessageEntity` rows; token rotation re-registers with the server. |
| **UnifiedPush** | `UnifiedPushTransport` (`.../transport/up`)        | Uses the `org.unifiedpush.android-connector`; registers against the server's `/up/{sub}/subscribe.json`. Source is user-selectable; if no UP distributor is installed the app guides the user to the WS fallback.              |

### Notification pipeline

A four-channel scheme mirrors the priority semantics:

| Priority             | Channel       | Behaviour                                                                  |
| -------------------- | ------------- | -------------------------------------------------------------------------- |
| `-2` (lowest)        | silent        | quiet                                                                      |
| `0` (normal)         | default       | default alert                                                              |
| `1` (high)           | high          | heads-up                                                                   |
| `2` (emergency)      | emergency     | `Importance.MAX` + full-screen intent + vibrate; carries an **Ack** action |

**Android 14 (API 34) full-screen intent:** the emergency channel requires
`USE_FULL_SCREEN_INTENT`. The app checks `canUseFullScreenIntent()`; if denied
it opens the system settings intent and falls back to a heads-up notification
with strong vibration until permission is granted. Source:
`android/.../notifications` (todo 32).

### Ack outbox

Acknowledging an emergency alert (`POST /1/receipts/{receipt}/acknowledge.json`
with the device secret) is queued through WorkManager so an offline ack is
retried with a network constraint and dismisses the notification on success.
A permanent `404` stops the queue and logs. Source: `android/.../outbox`
(todo 33).

### Data layer

Room database: `SubscriptionEntity` (server URL, user key, token, device id,
secret), `MessageEntity` (per-message, with `ack_state` and nullable
`receipt_id`), and a `SinceCursor` (per-subscription last id). Duplicate
message ids are replaced (`REPLACE`). Source: `android/.../data` (todo 28).

### UI

Compose screens for the subscription list, message detail (attachment + HTML),
server onboarding (URL + login -> device registration), and settings
(transport choice, battery-optimization disable prompt, FSI permission status,
test notification). Paparazzi golden screenshots cover the key screens.
Source: `android/.../ui` (todo 34).

### Build flavors / release

- Build flavors WS / FCM / UP (todo 30/31).
- F-Droid build metadata at `metadata/net.pushfree.android.yml`; release
  signing via env vars `KEYSTORE_PATH`/`KEYSTORE_PASS` (todo 35).
- Play Store supply pipeline structure (todo 49).

## Desktop app

Source: `desktop/`. Tauri 2 (Rust core + WebView UI), with
`tauri-plugin-tray-icon`, `tauri-plugin-autostart`, and
`tauri-plugin-notification`. Windows requires WebView2 (documented at first
run).

### WebSocket transport (direct)

The desktop client connects directly with `tokio-tungstenite` (not via a Tauri
plugin) to `GET /1/ws?since=`, using the same login-line protocol as Android's
WS flavor: 45 s server keepalive vs a >77 s read timeout, 30 s ping, Full-Jitter
reconnect (base 1 s, cap 60 s), and `since`-cursor replay on reconnect. Source:
`desktop/` WS client (todo 37).

### Notifications, dedup, ack

Local notifications are styled by priority; `send_id` deduplication uses a
persistent cursor so a redelivery is shown once; ack is reported to
`POST /1/receipts/{receipt}/acknowledge.json` and re-queued on failure. The
Rust `sendNotification` API is `void`/best-effort, so the desktop channel is a
"heads-up at the desk" channel, not a guaranteed wake (App Nap / Modern
Standby may delay it) — documented in `desktop/` (todo 38).

### Settings

Server URL, credentials, device-registration status, connection status, and
the `since` cursor are persisted with `tauri-plugin-store` and survive
restart; a corrupted store falls back to defaults and logs. Source (todo 39).

## Dashboard (embedded)

Source: `web/`. A Next.js app built as a static export (`output: 'export'`)
and embedded into the Go binary via `go:embed` at `/admin/` (todo 42).

Features (todo 41):

- Login (the `/v1/accounts/login` session cookie) and route guards.
- App-token management (`/v1/apps`).
- Quota display (reads `/1/apps/limits.json`).
- **Live message log** via SSE (`GET /1/sse`) — a send appears within seconds.
- Receipt browser (state, ack, callback history).
- Quiet-hours configuration (`PUT /v1/accounts/quiet-hours`).
- Admin view: user list and roles (admin accounts only).

Because the export is embedded, a plain `go build` produces a binary that
serves the real dashboard with no Node toolchain required at build time. The
embedded copy is refreshed from `web/out` by the server `Makefile` build
target (todo 42).

## Picking a transport

- **Android, push without Google:** WebSocket (foreground service) or
  UnifiedPush (with a distributor app installed).
- **Android, with Google services:** FCM is the most battery-friendly; WS is a
  reliable fallback.
- **Desktop:** WebSocket direct (the desktop is always online when in use).
- **Anything else:** the raw `GET /1/ws` / `GET /1/sse` endpoints are public
  (after device registration), so a custom client is straightforward — see
  [api.md](api.md#realtime-transports).
