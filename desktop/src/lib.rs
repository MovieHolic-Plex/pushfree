//! PushFree desktop client scaffold.
//!
//! Wires a minimal Tauri 2 app with a system-tray icon (show / hide / quit
//! menu items) and OS launch-at-login registration via the autostart plugin.
//! WebSocket client, notifications and settings persistence arrive in later
//! todos (37/38/39).

mod config;
pub mod notify;
pub mod ws;

use tauri::{
    menu::{IsMenuItem, Menu, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
    Manager,
};
use tauri_plugin_autostart::ManagerExt;

/// Entry point invoked by the binary `main` (and by the mobile entry point
/// when a mobile target is added later).
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        .plugin(tauri_plugin_notification::init())
        .setup(|app| {
            // --- Config sanity check -----------------------------------------
            // The identifier is compiled into the binary from tauri.conf.json;
            // assert it is valid reverse-DNS and matches our declared default.
            let identifier = app.config().identifier.as_str();
            if identifier != config::APP_IDENTIFIER {
                eprintln!(
                    "[pushfree] config identifier {identifier:?} differs from default {}",
                    config::APP_IDENTIFIER
                );
            }
            if !config::is_valid_identifier(identifier) {
                eprintln!("[pushfree] WARNING: invalid reverse-DNS identifier: {identifier}");
            }

            // --- Tray menu: built from the single source of truth -----------
            // `tauri::Builder::default()` uses the Wry runtime, so R = tauri::Wry.
            let menu_items: Vec<MenuItem<tauri::Wry>> = config::tray_menu_items()
                .into_iter()
                .map(|(id, label)| MenuItem::with_id(app, id, label, true, None::<&str>))
                .collect::<Result<_, _>>()?;
            let menu_refs: Vec<&dyn IsMenuItem<tauri::Wry>> = menu_items
                .iter()
                .map(|item| item as &dyn IsMenuItem<tauri::Wry>)
                .collect();
            let menu = Menu::with_items(app, &menu_refs)?;

            TrayIconBuilder::with_id("main")
                .icon(app_icon())
                .tooltip(config::WINDOW_TITLE)
                .menu(&menu)
                .show_menu_on_left_click(false)
                .on_menu_event(|app, event| match event.id.as_ref() {
                    "show" => {
                        if let Some(window) = app.get_webview_window(config::WINDOW_LABEL) {
                            let _ = window.show();
                            let _ = window.set_focus();
                        }
                    }
                    "hide" => {
                        if let Some(window) = app.get_webview_window(config::WINDOW_LABEL) {
                            let _ = window.hide();
                        }
                    }
                    "quit" => app.exit(0),
                    _ => {}
                })
                .on_tray_icon_event(|tray, event| {
                    if let TrayIconEvent::Click {
                        button: MouseButton::Left,
                        button_state: MouseButtonState::Up,
                        ..
                    } = event
                    {
                        let app = tray.app_handle();
                        if let Some(window) = app.get_webview_window(config::WINDOW_LABEL) {
                            if window.is_visible().unwrap_or(false) {
                                let _ = window.hide();
                            } else {
                                let _ = window.show();
                                let _ = window.set_focus();
                            }
                        }
                    }
                })
                .build(app)?;

            // --- Autostart registration (best-effort) ------------------------
            // A real app would gate this behind user settings (todo 39); the
            // wiring is exercised here so a successful launch registers the app
            // for OS launch-at-login. Failures (e.g. headless / no session) are
            // tolerated and must never abort startup.
            if let Err(err) = app.autolaunch().enable() {
                eprintln!("[pushfree] autostart enable failed: {err}");
            }

            // --- Notification + ack pipeline (todo 38) ----------------------
            // Production sink: tauri-plugin-notification. Server config arrives
            // from env vars as a minimal bridge until todo 39 adds a persisted
            // settings store; with no config the tray app still runs and the
            // pipeline stays inert.
            wire_notifications(app);

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running PushFree tauri application");
}

/// A small solid-colour RGBA icon built from raw pixels.
///
/// No PNG/ICO decoding path is needed (which keeps `cargo build` hermetic — no
/// `image-png`/`image-ico` feature). `icons/icon.ico` is still shipped because
/// `tauri-build` requires it on Windows for the exe resource file; a real brand
/// icon replaces this set in a later todo.
fn app_icon() -> tauri::image::Image<'static> {
    const W: u32 = 16;
    const H: u32 = 16;
    // 4 bytes per pixel: RGBA. PushFree blue, fully opaque.
    const PIXEL: [u8; 4] = [0x32, 0x6C, 0xE0, 0xFF];
    let mut rgba = Vec::with_capacity((W as usize * H as usize) * 4);
    for _ in 0..(W * H) {
        rgba.extend_from_slice(&PIXEL);
    }
    tauri::image::Image::new(rgba.leak(), W, H)
}

// ---------------------------------------------------------------------------
// Notification + ack wiring (todo 38)
// ---------------------------------------------------------------------------

/// Minimal server config sourced from the environment until todo 39 adds a
/// persisted settings store. `http_base` feeds the ack HTTP client; `ws_url` is
/// the scheme-converted WebSocket URL for the live receive loop.
struct EnvConfig {
    ws_url: String,
    http_base: String,
    device_id: String,
    secret: String,
}

fn read_env_config() -> Option<EnvConfig> {
    let http_base = std::env::var("PUSHFREE_SERVER_URL").ok()?;
    let device_id = std::env::var("PUSHFREE_DEVICE_ID").ok()?;
    let secret = std::env::var("PUSHFREE_SECRET").ok()?;
    if http_base.is_empty() || device_id.is_empty() || secret.is_empty() {
        return None;
    }
    Some(EnvConfig {
        ws_url: http_to_ws(&http_base),
        http_base,
        device_id,
        secret,
    })
}

/// Convert an `http(s)://` server base to the `ws(s)://` form the WS client
/// needs. A trailing slash is tolerated.
fn http_to_ws(http_base: &str) -> String {
    let trimmed = http_base.trim_end_matches('/');
    if let Some(rest) = trimmed.strip_prefix("https://") {
        format!("wss://{rest}")
    } else if let Some(rest) = trimmed.strip_prefix("http://") {
        format!("ws://{rest}")
    } else {
        trimmed.to_string()
    }
}

/// Build the production notification sink, ack client, and pipeline; spawn the
/// ack reporter and the WS receive loop that feeds message events into the
/// pipeline. With no env config the function logs and returns (the tray app
/// still runs; todo 39 starts the loop from persisted settings).
fn wire_notifications(app: &tauri::App<tauri::Wry>) {
    use std::sync::Arc;

    let Some(cfg) = read_env_config() else {
        eprintln!(
            "[pushfree] no server config (set PUSHFREE_SERVER_URL, \
             PUSHFREE_DEVICE_ID, PUSHFREE_SECRET); WS loop not started"
        );
        return;
    };

    let sink: Arc<dyn notify::NotifySink> = Arc::new(notify::ToastSink::new(app.handle().clone()));
    let ack_client = match notify::HttpAckClient::new(cfg.http_base.clone(), cfg.secret.clone()) {
        Ok(c) => Arc::new(c),
        Err(err) => {
            eprintln!("[pushfree] ack client config error: {err}");
            return;
        }
    };

    let (ack_tx, ack_rx) = tokio::sync::mpsc::channel::<i64>(256);
    let reporter = notify::AckReporter::new(ack_rx, ack_client, notify::DEFAULT_ACK_RETRY_DELAY);
    tauri::async_runtime::spawn(reporter.run());

    let pipeline = Arc::new(notify::Pipeline::new(
        notify::Dedup::new(None),
        sink,
        ack_tx,
    ));

    // Drive decoded WS messages into the pipeline (notify + ack).
    let ws_cfg = ws::WsConfig::new(cfg.ws_url, cfg.device_id, cfg.secret);
    let ws_client = match ws::Client::new(ws_cfg) {
        Ok(c) => Arc::new(c),
        Err(err) => {
            eprintln!("[pushfree] WS client config error: {err}");
            return;
        }
    };
    let pipeline_for_drive = pipeline.clone();
    tauri::async_runtime::spawn(async move {
        let (tx, mut rx) = tokio::sync::mpsc::channel::<ws::Event>(64);
        // Sidecar: decode message frames into pipeline.handle calls.
        let drive = tauri::async_runtime::spawn(async move {
            while let Some(ev) = rx.recv().await {
                if let ws::Event::Message(m) = ev {
                    pipeline_for_drive.handle(&m).await;
                }
            }
        });
        // App-lifetime run; the shutdown signal never resolves, so the loop runs
        // until the Tauri runtime is dropped on exit.
        ws_client.run(tx, std::future::pending::<()>()).await;
        drive.abort();
    });

    // Expose the pipeline as managed state for todo 39 (settings UI / control).
    app.manage(pipeline);
}
