//! Pure configuration helpers.
//!
//! These intentionally avoid Tauri types so they are unit-testable in a
//! headless environment (`cargo test`) without booting a window or webview.

/// Reverse-DNS application identifier, also set in `tauri.conf.json`.
pub const APP_IDENTIFIER: &str = "net.pushfree.desktop";

/// Label of the primary window, declared in `tauri.conf.json` under
/// `app.windows`. The tray menu and click handler target this label.
pub const WINDOW_LABEL: &str = "main";

/// Window / product title.
pub const WINDOW_TITLE: &str = "PushFree";

/// The system-tray menu specification as `(id, label)` pairs, in display order.
///
/// Kept as a pure function so the tray contract (which items exist and in what
/// order) is asserted by unit tests independently of the Tauri runtime.
pub fn tray_menu_items() -> Vec<(&'static str, &'static str)> {
    vec![
        ("show", "Show"),
        ("hide", "Hide"),
        ("quit", "Quit"),
    ]
}

/// Validate a reverse-DNS identifier (e.g. `net.pushfree.desktop`).
///
/// Accepts any string with at least two non-empty dot-separated labels. Used to
/// guard configuration parsing in later todos; exercised here so `cargo test`
/// runs a non-empty suite.
pub fn is_valid_identifier(id: &str) -> bool {
    if id.is_empty() {
        return false;
    }
    let labels: Vec<&str> = id.split('.').collect();
    labels.len() >= 2 && labels.iter().all(|label| !label.is_empty())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tray_menu_has_show_hide_quit_in_order() {
        let items = tray_menu_items();
        let ids: Vec<&str> = items.iter().map(|(id, _)| *id).collect();
        assert_eq!(ids, vec!["show", "hide", "quit"], "tray item order/ids");
        assert!(
            items.iter().all(|(_, label)| !label.is_empty()),
            "every tray label must be non-empty"
        );
        // Ensure the ids the Rust menu handler switches on actually exist.
        assert!(items.iter().any(|(id, _)| *id == "quit"));
    }

    #[test]
    fn identifier_validation() {
        assert!(is_valid_identifier(APP_IDENTIFIER));
        assert!(is_valid_identifier("a.b"));
        assert!(is_valid_identifier("com.example.my_app"));
        // Rejected shapes.
        assert!(!is_valid_identifier(""));
        assert!(!is_valid_identifier("pushfree"), "single label rejected");
        assert!(!is_valid_identifier("net."), "trailing dot -> empty label");
        assert!(!is_valid_identifier(".net"), "leading dot -> empty label");
    }
}
