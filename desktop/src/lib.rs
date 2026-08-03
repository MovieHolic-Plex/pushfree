//! PushFree desktop client scaffold.
//!
//! Wires a Tauri 2 app with:
//! - A system-tray icon (Open / Reconnect / Quit menu items).
//! - OS launch-at-login registration gated on the persisted `autolaunch`
//!   setting (no longer unconditionally enabled at startup).
//! - A persisted JSON settings store for server URL, device credentials,
//!   transport, and the autolaunch toggle (see [`settings`]).
//! - The WebSocket receive + notification + ack pipeline (todos 37/38) driven
//!   from the persisted settings, with a tray "Reconnect" action and an
//!   `invoke` command that restart the WS loop on demand.

mod config;
pub mod e2ee;
pub mod notify;
pub mod settings;
pub mod ws;

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use tauri::{
    menu::{IsMenuItem, Menu, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
    Manager,
};
use tauri_plugin_autostart::ManagerExt;

use settings::Settings;

/// Entry point invoked by the binary `main`.
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        .plugin(tauri_plugin_notification::init())
        .invoke_handler(tauri::generate_handler![
            cmd_get_settings,
            cmd_save_settings,
            cmd_reconnect_ws,
        ])
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
                    "open" => {
                        if let Some(window) = app.get_webview_window(config::WINDOW_LABEL) {
                            let _ = window.show();
                            let _ = window.set_focus();
                        }
                    }
                    "reconnect" => {
                        let store = app.state::<SettingsStore>();
                        let s = store.get();
                        let config_dir = store.config_dir.clone();
                        app.state::<WsController>().restart(app, &s, &config_dir);
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

            // --- Settings + persistence (todo 39) ----------------------------
            let config_dir = resolve_config_dir(app);
            let settings_path = Settings::path_in(&config_dir);
            let settings = Settings::load(&settings_path);
            eprintln!(
                "[pushfree] settings loaded from {} (server_url={}, device_id={}, \
                 autolaunch={})",
                settings_path.display(),
                display_or_unset(&settings.server_url),
                display_or_unset(&settings.device_id),
                settings.autolaunch,
            );

            // --- Autostart: apply the persisted toggle (not unconditional) ----
            apply_autolaunch(app.handle(), settings.autolaunch);

            // --- Managed state + WS pipeline ---------------------------------
            let handle = app.handle().clone();
            app.manage(SettingsStore::new(settings.clone(), config_dir.clone()));
            app.manage(WsController::new());
            app.state::<WsController>()
                .restart(&handle, &settings, &config_dir);

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running PushFree tauri application");
}

// ---------------------------------------------------------------------------
// Managed state: settings store
// ---------------------------------------------------------------------------

/// Thread-safe holder for the live settings plus the config directory they (and
/// the WS cursor / dedup log) live in. Managed via `app.manage` so the
/// `invoke` commands and tray handler can read/update settings.
pub struct SettingsStore {
    settings: Mutex<Settings>,
    config_dir: PathBuf,
}

impl SettingsStore {
    pub fn new(settings: Settings, config_dir: PathBuf) -> Self {
        SettingsStore {
            settings: Mutex::new(settings),
            config_dir,
        }
    }

    /// Path to the settings JSON file.
    pub fn settings_path(&self) -> PathBuf {
        Settings::path_in(&self.config_dir)
    }

    /// A snapshot of the current settings.
    pub fn get(&self) -> Settings {
        self.settings
            .lock()
            .expect("settings lock poisoned")
            .clone()
    }

    /// Replace the in-memory settings (does not persist to disk on its own;
    /// callers that want durability call [`Settings::save`] first).
    pub fn put(&self, settings: Settings) {
        *self.settings.lock().expect("settings lock poisoned") = settings;
    }
}

// ---------------------------------------------------------------------------
// Managed state: WS runtime controller (reconnect support)
// ---------------------------------------------------------------------------

/// Owns the background WS receive task so the tray "Reconnect" item and the
/// `reconnect_ws` command can abort and restart it. The notification sink, ack
/// reporter, dedup set, and pipeline are rebuilt on each restart from the
/// current settings, so a settings change (new server URL / new credentials)
/// takes effect immediately without restarting the app.
pub struct WsController {
    task: Mutex<Option<tauri::async_runtime::JoinHandle<()>>>,
}

impl WsController {
    pub fn new() -> Self {
        WsController {
            task: Mutex::new(None),
        }
    }

    /// Abort any running WS task. Safe to call when none is running.
    pub fn abort(&self) {
        if let Some(handle) = self.task.lock().expect("ws task lock poisoned").take() {
            handle.abort();
        }
    }

    /// Abort any prior WS task and start a fresh receive loop from `settings`.
    /// If the settings are incomplete (no URL / credentials) the function logs
    /// and returns without starting a task — the tray app still runs and the
    /// settings UI can be used to configure the server.
    pub fn restart(
        &self,
        app: &tauri::AppHandle<tauri::Wry>,
        settings: &Settings,
        config_dir: &Path,
    ) {
        self.abort();

        let Some(cfg) = build_runtime_config(settings) else {
            eprintln!(
                "[pushfree] incomplete server config (server_url/device_id/secret); \
                 WS loop not started"
            );
            return;
        };

        let sink: Arc<dyn notify::NotifySink> = Arc::new(notify::ToastSink::new(app.clone()));
        let ack_client = match notify::HttpAckClient::new(cfg.http_base.clone(), cfg.secret.clone())
        {
            Ok(c) => Arc::new(c),
            Err(err) => {
                eprintln!("[pushfree] ack client config error: {err}");
                return;
            }
        };

        let cursor_file = config_dir.join("cursor.txt");
        let seen_file = config_dir.join("seen.log");

        let mut ws_config = ws::WsConfig::new(cfg.ws_url, cfg.device_id, cfg.secret);
        ws_config.cursor_path = Some(cursor_file);
        let ws_client = match ws::Client::new(ws_config) {
            Ok(c) => Arc::new(c),
            Err(err) => {
                eprintln!("[pushfree] WS client config error: {err}");
                return;
            }
        };

        let retry_delay = notify::DEFAULT_ACK_RETRY_DELAY;
        // Hoist the E2EE key clone before the 'static spawned task: the task
        // cannot borrow `settings` (a function-parameter reference).
        let e2ee_key = settings.e2ee_key.clone();
        let handle = tauri::async_runtime::spawn(async move {
            // Ack reporter: drains the pipeline's ack queue with retry. Spawned
            // inside the WS task so that when this task is aborted the pipeline
            // (and its ack sender) is dropped, the queue closes, and the
            // reporter exits naturally.
            let (ack_tx, ack_rx) = tokio::sync::mpsc::channel::<i64>(256);
            let reporter = notify::AckReporter::new(ack_rx, ack_client, retry_delay);
            tauri::async_runtime::spawn(reporter.run());

            let pipeline = Arc::new(
                notify::Pipeline::new(notify::Dedup::new(Some(seen_file)), sink, ack_tx)
                    .with_e2ee_key(Some(e2ee_key)),
            );

            // Drive decoded WS message frames into the pipeline.
            let pipeline_for_drive = pipeline.clone();
            let (tx, mut rx) = tokio::sync::mpsc::channel::<ws::Event>(64);
            let drive = tauri::async_runtime::spawn(async move {
                while let Some(ev) = rx.recv().await {
                    if let ws::Event::Message(m) = ev {
                        pipeline_for_drive.handle(&m).await;
                    }
                }
            });

            // Runs until the Tauri runtime is dropped on exit, or until this
            // task is aborted by a subsequent restart()/abort() call.
            ws_client.run(tx, std::future::pending::<()>()).await;
            drive.abort();
        });

        *self.task.lock().expect("ws task lock poisoned") = Some(handle);
    }
}

impl Default for WsController {
    fn default() -> Self {
        Self::new()
    }
}

// ---------------------------------------------------------------------------
// Tauri commands (settings UI surface)
// ---------------------------------------------------------------------------

/// Return the current settings to the webview.
#[tauri::command]
fn cmd_get_settings(state: tauri::State<'_, SettingsStore>) -> Settings {
    state.get()
}

/// Validate, persist, and apply new settings. On success the WS loop is
/// restarted with the new config and the autolaunch toggle is reconciled with
/// the OS. Returns a human-readable error string on failure.
#[tauri::command]
fn cmd_save_settings(
    app: tauri::AppHandle<tauri::Wry>,
    new_settings: Settings,
    store: tauri::State<'_, SettingsStore>,
    ws: tauri::State<'_, WsController>,
) -> Result<(), String> {
    new_settings.validate().map_err(|e| e.to_string())?;
    new_settings
        .save(&store.settings_path())
        .map_err(|e| e.to_string())?;
    apply_autolaunch(&app, new_settings.autolaunch);
    let config_dir = store.config_dir.clone();
    ws.restart(&app, &new_settings, &config_dir);
    store.put(new_settings);
    Ok(())
}

/// Restart the WS receive loop with the current (already-persisted) settings.
#[tauri::command]
fn cmd_reconnect_ws(
    app: tauri::AppHandle<tauri::Wry>,
    store: tauri::State<'_, SettingsStore>,
    ws: tauri::State<'_, WsController>,
) -> Result<(), String> {
    let s = store.get();
    let config_dir = store.config_dir.clone();
    ws.restart(&app, &s, &config_dir);
    Ok(())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// Resolve the OS app-config directory, creating it if needed. Falls back to a
/// temp-dir subdir if the platform path cannot be resolved (very unusual).
fn resolve_config_dir(app: &tauri::App<tauri::Wry>) -> PathBuf {
    let dir = match app.path().app_config_dir() {
        Ok(p) => p,
        Err(e) => {
            eprintln!("[pushfree] cannot resolve config dir: {e}; using temp dir");
            std::env::temp_dir().join("pushfree")
        }
    };
    if let Err(e) = std::fs::create_dir_all(&dir) {
        eprintln!("[pushfree] cannot create config dir {}: {e}", dir.display());
    }
    dir
}

/// Reconcile the OS launch-at-login state with `enabled`. Best-effort: errors
/// are logged and never propagated (a headless CI session cannot register
/// autostart and must not fail startup).
fn apply_autolaunch(app: &tauri::AppHandle<tauri::Wry>, enabled: bool) {
    let manager = app.autolaunch();
    let currently = manager.is_enabled().unwrap_or(false);
    if enabled && !currently {
        if let Err(e) = manager.enable() {
            eprintln!("[pushfree] autolaunch enable failed: {e}");
        }
    } else if !enabled && currently {
        if let Err(e) = manager.disable() {
            eprintln!("[pushfree] autolaunch disable failed: {e}");
        }
    }
}

/// The connection parameters derived from persisted settings. `None` when the
/// settings are incomplete (no URL / credentials), meaning the WS loop should
/// not start.
struct RuntimeConfig {
    ws_url: String,
    http_base: String,
    device_id: String,
    secret: String,
}

fn build_runtime_config(s: &Settings) -> Option<RuntimeConfig> {
    if s.server_url.is_empty() || s.device_id.is_empty() || s.secret.is_empty() {
        return None;
    }
    Some(RuntimeConfig {
        ws_url: http_to_ws(&s.server_url),
        http_base: s.server_url.clone(),
        device_id: s.device_id.clone(),
        secret: s.secret.clone(),
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

/// `<unset>` placeholder for log lines when a field is empty.
fn display_or_unset(s: &str) -> &str {
    if s.is_empty() {
        "<unset>"
    } else {
        s
    }
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn http_to_ws_converts_schemes() {
        assert_eq!(http_to_ws("https://example.com"), "wss://example.com");
        assert_eq!(http_to_ws("http://localhost:2586"), "ws://localhost:2586");
        // trailing slash tolerated
        assert_eq!(http_to_ws("https://example.com/"), "wss://example.com");
    }

    #[test]
    fn build_runtime_config_requires_all_fields() {
        let mut s = Settings::default();
        assert!(build_runtime_config(&s).is_none(), "all-empty -> None");

        s.server_url = "https://x.example".into();
        assert!(build_runtime_config(&s).is_none(), "missing creds -> None");

        s.device_id = "dev".into();
        assert!(build_runtime_config(&s).is_none(), "missing secret -> None");

        s.secret = "sec".into();
        let cfg = build_runtime_config(&s).expect("complete -> Some");
        assert_eq!(cfg.ws_url, "wss://x.example");
        assert_eq!(cfg.http_base, "https://x.example");
        assert_eq!(cfg.device_id, "dev");
        assert_eq!(cfg.secret, "sec");
    }

    #[test]
    fn ws_controller_new_has_no_task() {
        let ctrl = WsController::new();
        assert!(ctrl.task.lock().unwrap().is_none());
        // abort on an empty controller must not panic.
        ctrl.abort();
    }
}
