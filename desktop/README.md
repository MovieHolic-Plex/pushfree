# PushFree Desktop (Tauri 2)

Native Windows / macOS / Linux desktop client for PushFree, built with
[Rust](https://rust-lang.org) and [Tauri 2](https://tauri.app).

<p align="center">
  <img src="icons/icon.png" width="128" alt="PushFree Desktop Icon">
</p>

## Features

- **Direct WebSocket transport** — persistent connection to the PushFree
  server with full-jitter exponential backoff reconnect.
- **Native OS notifications** — system tray icon, desktop notifications with
  dedup (crash-safe append-only log, so a mid-session crash never double-fires).
- **Emergency (priority-2) support** — emergency alerts are NOT auto-acked
  (matching Pushover semantics: the alert repeats until a human explicitly
  acknowledges). The ack infrastructure is wired and ready for a future
  Tauri notification action / dialog trigger.
- **End-to-end encryption** — AES-256-CBC + HMAC decryption at ingest, before
  the notification is shown. Byte-faithful parity with the Go reference.
- **Settings persistence** — server URL, credentials, and optional E2EE key
  are persisted to disk and threaded through the WS controller.
- **Autostart** — registers OS launch-at-login (Windows Run key, macOS
  LaunchAgent, Linux `.desktop` entry).

## Layout

```
desktop/
  Cargo.toml                 # crate `pushfree-desktop` (tauri 2.x + autostart)
  build.rs                   # tauri_build::build()
  tauri.conf.json            # identifier net.pushfree.desktop
  capabilities/default.json  # Tauri 2 ACL
  src/
    main.rs                  # binary entry (calls lib::run)
    lib.rs                   # tray + autostart + WS controller wiring
    ws.rs                    # WebSocket client (reconnect/backoff/keepalive)
    notify/                  # notification pipeline (dedup + ack reporting)
      mod.rs
      tests.rs
    e2ee.rs                  # E2EE decrypt (AES-CBC + HMAC)
    settings.rs              # persisted settings (unit-tested)
    config.rs                # runtime config helpers
  icons/                     # app icons (32/128/128@2x)
  dist/index.html            # frontend placeholder
```

## Build & Test

```sh
cd desktop
cargo build              # compile
cargo test               # 53 unit tests
cargo tauri build        # produce native bundle (needs WebView2 on Windows)
```

All tests are headless — no display or WebView2 runtime needed for `cargo test`.
