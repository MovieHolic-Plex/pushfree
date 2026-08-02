# PushFree Desktop (Tauri 2)

Native Windows / macOS / Linux desktop client for PushFree, built with
[Tauri 2](https://tauri.app/). This package is the **scaffold** (todo 36): it
brings up a minimal window, a system-tray icon, and OS launch-at-login
registration. The WebSocket transport, local notifications and settings
persistence arrive in later todos (37 / 38 / 39).

## Layout

```
desktop/
  Cargo.toml            # crate `pushfree-desktop` (tauri 2.x + autostart)
  build.rs              # tauri_build::build() — validates + codegens config
  tauri.conf.json        # identifier net.pushfree.desktop, window title "PushFree"
  capabilities/default.json  # Tauri 2 ACL: core:default for the "main" window
  src/main.rs           # binary entry (calls lib::run)
  src/lib.rs            # tray (show/hide/quit) + autostart wiring + icon
  src/config.rs         # pure config helpers (unit-tested)
  dist/index.html       # static frontend placeholder (frontendDist target)
```

## Tray icon (core capability)

In Tauri 2 the system tray is a **core feature**, not a separate plugin crate.
The task brief named `tauri-plugin-tray-icon`, but that crate does **not exist**
on crates.io (`{"errors":[{"detail":"crate 'tauri-plugin-tray-icon' does not
exist"}]}`). The canonical Tauri 2 path is the `tray-icon` Cargo feature on the
`tauri` crate itself, which is what this scaffold uses. The menu exposes three
items wired in `src/lib.rs`:

| Menu item | Behaviour |
|-----------|-----------|
| Show      | `window.show()` + `set_focus()` |
| Hide      | `window.hide()` |
| Quit      | `app.exit(0)` |

Left-clicking the tray icon toggles window visibility.

## Autostart

Registered via [`tauri-plugin-autostart`](https://docs.rs/tauri-plugin-autostart)
(`init(MacosLauncher::LaunchAgent, None)`). On startup the scaffold calls
`app.autolaunch().enable()` best-effort — Windows writes the `Run` registry
key, macOS installs a LaunchAgent, Linux drops an autostart `.desktop` entry.
Failures are logged and tolerated; they must never block startup. A real app
will gate this behind a settings toggle (todo 39).

## Headless build & test

Everything is verified without a display:

```pwsh
cd desktop
cargo build        # exit 0 (first compile downloads many crates — be patient)
cargo test         # exit 0, >=1 unit test runs (src/config.rs)
cargo clippy -D warnings   # exit 0
```

`cargo build`/`cargo test` never launch a window, so no WebView2 runtime is
needed for CI. The `tauri::generate_context!()` macro does read
`tauri.conf.json` at compile time, so a malformed config fails `cargo build`
with a config-naming error (see evidence file).

## Manual bundle QA

WebView2 runtime is assumed present (bundled on Windows 10/11; if missing,
install the Evergreen Bootstrapper from
<https://developer.microsoft.com/en-us/microsoft-edge/webview2/>).

```pwsh
cd desktop
# Generate the icon set first (one source PNG -> all required sizes):
#   cargo install tauri-cli --version "^2.11"
#   cargo tauri icon path/to/icon.png
cargo tauri build --debug
```

The scaffold intentionally ships **no** bundle icon (`bundle.icon` is unset) to
keep `cargo build` hermetic; run `cargo tauri icon` before bundling, as shown
above. Expected runtime: a "PushFree" window plus a tray icon with Show/Hide/Quit.

## What is NOT here (later todos)

- WebSocket client direct to the PushFree server (todo 37)
- Local notifications + ack reporting + dedup (todo 38)
- Settings UI + persistence (todo 39)
