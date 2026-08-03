//! PushFree desktop client scaffold.
//!
//! Wires a minimal Tauri 2 app with a system-tray icon (show / hide / quit
//! menu items) and OS launch-at-login registration via the autostart plugin.
//! WebSocket client, notifications and settings persistence arrive in later
//! todos (37/38/39).

mod config;
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
