//! Persisted user settings: server URL, device credentials, transport, and the
//! launch-at-login toggle.
//!
//! # Design (plan todo 39; research EB/A3-tauri.md)
//! - Storage is a plain JSON file in the OS app-config directory, NOT
//!   `tauri-plugin-store`. A plain file needs no AppHandle, no extra crate, and
//!   no async runtime, so the whole persistence layer is unit-testable
//!   headlessly (`cargo test settings`) with a temp path. This is the explicit
//!   "pick one" decision from the task spec.
//! - Writes are atomic (write-to-tmp + rename) so a crash mid-save never
//!   truncates the settings; at worst the previous file remains. This mirrors
//!   the WS cursor write strategy in `ws::Cursor`.
//! - The struct carries `#[serde(default)]` so a missing or partial file (from
//!   an older build, or a hand edit) loads with sensible defaults instead of
//!   failing — the "corrupt settings -> defaults + log" QA path.
//!
//! # Connection to the rest of the app
//! `lib::run` loads settings at startup, gates the autostart plugin on
//! `autolaunch`, and feeds `server_url`/`device_id`/`secret` into the WS client
//! and ack HTTP client. The Tauri commands `get_settings` / `save_settings`
//! expose the store to the webview UI (todo 34-style settings panel).

use std::io::Write;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

/// File name used inside the app-config directory.
pub const FILE_NAME: &str = "settings.json";

/// The single transport the desktop client supports today. Modelled as an enum
/// (not a bool) so future transports (e.g. SSE fallback) are additive and so
/// the persisted value is self-describing.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Transport {
    #[default]
    WebSocket,
}

/// Persisted user settings. Every field defaults to an "unconfigured" value so
/// a fresh install boots without error and shows the first-run setup flow.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Settings {
    /// Server base URL, e.g. `https://push.example.com`. Empty until the user
    /// configures it. Must be `http(s)://host[:port]` when set.
    pub server_url: String,
    /// Registered device id (todo 13 device login).
    pub device_id: String,
    /// Device secret used for WS login + ack auth.
    pub secret: String,
    /// Transport selection (only WebSocket today).
    pub transport: Transport,
    /// Whether the app registers for OS launch-at-login.
    pub autolaunch: bool,
    /// E2EE key (64-char hex) for decrypting `encrypted=1` message fields
    /// (todo 44). Empty until the user configures it out-of-band. When empty,
    /// encrypted messages surface as `[undecryptable]` (safe error state).
    pub e2ee_key: String,
}

/// A validation or persistence error surfaced to the UI.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SettingsError {
    /// `server_url` was set but is not a valid `http(s)://host[:port]`.
    InvalidServerUrl,
    /// The settings file could not be written.
    SaveFailed(String),
}

impl std::fmt::Display for SettingsError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SettingsError::InvalidServerUrl => {
                write!(
                    f,
                    "server URL must be http:// or https:// followed by a host"
                )
            }
            SettingsError::SaveFailed(m) => write!(f, "failed to save settings: {m}"),
        }
    }
}

impl std::error::Error for SettingsError {}

impl Settings {
    /// Resolve `<config_dir>/settings.json`.
    pub fn path_in(config_dir: &Path) -> PathBuf {
        config_dir.join(FILE_NAME)
    }

    /// Load settings from `path`. A missing file yields defaults (first run);
    /// a malformed file yields defaults + a logged warning (corrupt-settings
    /// recovery). Never panics.
    pub fn load(path: &Path) -> Settings {
        match std::fs::read(path) {
            Ok(bytes) => match serde_json::from_slice::<Settings>(&bytes) {
                Ok(s) => s,
                Err(e) => {
                    eprintln!(
                        "[pushfree/settings] corrupt settings at {}: {e}; using defaults",
                        path.display()
                    );
                    Settings::default()
                }
            },
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Settings::default(),
            Err(e) => {
                eprintln!(
                    "[pushfree/settings] read error at {}: {e}; using defaults",
                    path.display()
                );
                Settings::default()
            }
        }
    }

    /// Validate then atomically persist. On success the file holds the exact
    /// JSON that [`Settings::load`] will round-trip back.
    pub fn save(&self, path: &Path) -> Result<(), SettingsError> {
        self.validate()?;
        Self::write_atomic(path, self).map_err(|e| SettingsError::SaveFailed(e.to_string()))
    }

    /// Validate the settings without writing. An empty `server_url` is treated
    /// as "unconfigured" and is valid (first-run state); a non-empty one must
    /// parse as an `http(s)://host[:port]` URL. Credentials may be empty
    /// (the user can save a URL before device registration completes).
    pub fn validate(&self) -> Result<(), SettingsError> {
        if self.server_url.is_empty() {
            return Ok(());
        }
        if !is_valid_server_url(&self.server_url) {
            return Err(SettingsError::InvalidServerUrl);
        }
        Ok(())
    }

    /// Atomic write: serialise to `<name>.tmp`, `sync_all`, then rename over
    /// the target. The rename is atomic on the same filesystem (POSIX rename /
    /// Win32 MoveFileEx-with-replace), so a crash mid-write never leaves a
    /// truncated settings file.
    fn write_atomic(path: &Path, settings: &Settings) -> std::io::Result<()> {
        if let Some(parent) = path.parent() {
            if !parent.as_os_str().is_empty() {
                std::fs::create_dir_all(parent)?;
            }
        }
        let tmp = path.with_file_name(format!(
            "{}.tmp",
            path.file_name()
                .and_then(|n| n.to_str())
                .unwrap_or("settings")
        ));
        let bytes = serde_json::to_vec_pretty(settings)?;
        {
            let mut f = std::fs::File::create(&tmp)?;
            f.write_all(&bytes)?;
            f.sync_all()?;
        }
        std::fs::rename(&tmp, path)
    }
}

/// Validate an `http(s)://host[:port]` server URL. Pure so it is unit-tested
/// without any I/O. Accepts `http://localhost`, `https://push.example.com`,
/// `https://host:2586`. Rejects empty, wrong scheme, or missing host.
pub fn is_valid_server_url(url: &str) -> bool {
    let trimmed = url.trim();
    if trimmed.is_empty() {
        return false;
    }
    let rest = match trimmed {
        s if let Some(r) = s.strip_prefix("https://") => r,
        s if let Some(r) = s.strip_prefix("http://") => r,
        _ => return false,
    };
    // Authority runs up to the first path/query/fragment delimiter.
    let authority_end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let authority = &rest[..authority_end];
    // Host is after the last '@' (drop any userinfo).
    let host_with_port = authority.rsplit('@').next().unwrap_or("");
    let host = host_with_port
        .rsplit_once(':')
        .map(|(h, _)| h)
        .unwrap_or(host_with_port);
    !host.is_empty()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering};

    /// A unique temp path per test call so parallel `cargo test` threads never
    /// collide. Cleaned up (best-effort) at the end of each test.
    fn temp_settings_path(name: &str) -> PathBuf {
        static COUNTER: AtomicU64 = AtomicU64::new(0);
        let n = COUNTER.fetch_add(1, Ordering::Relaxed);
        let pid = std::process::id();
        let dir = std::env::temp_dir().join(format!("pushfree-settings-test-{pid}"));
        let _ = std::fs::create_dir_all(&dir);
        dir.join(format!("{name}-{n}.json"))
    }

    fn cleanup(path: &Path) {
        let _ = std::fs::remove_file(path);
        let _ = std::fs::remove_file(path.with_extension("json.tmp"));
    }

    // --- URL validation -----------------------------------------------------

    #[test]
    fn settings_valid_server_urls() {
        for ok in [
            "http://localhost",
            "https://push.example.com",
            "https://example.com:2586",
            "http://192.168.1.10:8080",
            "https://user:pass@host.io/path",
            "http://host/",
            "  https://trim.example  ",
        ] {
            assert!(is_valid_server_url(ok), "should accept {ok:?}");
        }
    }

    #[test]
    fn settings_rejects_invalid_server_urls() {
        for bad in [
            "",
            "   ",
            "ftp://host",
            "push.example.com",
            "http://",
            "https:///",
            "not a url",
            "://no-scheme",
        ] {
            assert!(!is_valid_server_url(bad), "should reject {bad:?}");
        }
    }

    // --- round-trip persistence --------------------------------------------

    #[test]
    fn settings_round_trip_persists_all_fields() {
        let path = temp_settings_path("roundtrip");
        cleanup(&path);

        let original = Settings {
            server_url: "https://push.example.com:2586".into(),
            device_id: "dev-abc123".into(),
            secret: "s3cr3t".into(),
            transport: Transport::WebSocket,
            autolaunch: true,
            e2ee_key: "deadbeef".repeat(8),
        };
        original.save(&path).expect("save should succeed");

        // The file must exist and contain valid JSON before we reload.
        assert!(path.exists(), "settings file should exist after save");

        let loaded = Settings::load(&path);
        assert_eq!(loaded, original, "round-trip must preserve every field");

        cleanup(&path);
    }

    #[test]
    fn settings_save_rejects_invalid_url_with_message() {
        let path = temp_settings_path("invalid");
        cleanup(&path);

        let bad = Settings {
            server_url: "ftp://nope".into(),
            device_id: "d".into(),
            secret: "s".into(),
            ..Settings::default()
        };
        let err = bad.save(&path).expect_err("invalid URL must be rejected");
        assert_eq!(err, SettingsError::InvalidServerUrl);
        assert!(
            !path.exists(),
            "rejected settings must not be written to disk"
        );
        // The error message must mention the URL problem for the UI.
        assert!(err.to_string().to_lowercase().contains("url"));

        cleanup(&path);
    }

    #[test]
    fn settings_empty_url_is_valid_unconfigured_state() {
        // First-run / unconfigured state: empty URL is OK, not an error.
        let s = Settings::default();
        assert!(s.validate().is_ok(), "default settings must validate");
    }

    // --- corrupt / missing file recovery -----------------------------------

    #[test]
    fn settings_load_missing_file_returns_defaults() {
        let path = temp_settings_path("missing");
        cleanup(&path);

        let loaded = Settings::load(&path);
        assert_eq!(loaded, Settings::default());

        cleanup(&path);
    }

    #[test]
    fn settings_load_corrupt_file_returns_defaults() {
        let path = temp_settings_path("corrupt");
        cleanup(&path);
        std::fs::write(&path, b"{ this is not : valid json }").unwrap();

        let loaded = Settings::load(&path);
        assert_eq!(loaded, Settings::default(), "corrupt file -> defaults");

        cleanup(&path);
    }

    #[test]
    fn settings_load_partial_file_uses_field_defaults() {
        // An older build that only knew `server_url` must still load, filling
        // the unknown fields with defaults.
        let path = temp_settings_path("partial");
        cleanup(&path);
        std::fs::write(&path, b"{\"server_url\":\"https://old.example\"}").unwrap();

        let loaded = Settings::load(&path);
        assert_eq!(loaded.server_url, "https://old.example");
        assert_eq!(loaded.device_id, "");
        assert_eq!(loaded.secret, "");
        assert_eq!(loaded.transport, Transport::WebSocket);
        assert!(!loaded.autolaunch);

        cleanup(&path);
    }

    // --- atomic write guarantees -------------------------------------------

    #[test]
    fn settings_save_creates_parent_dir() {
        let dir =
            std::env::temp_dir().join(format!("pushfree-settings-nested-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let path = dir.join("deep").join("settings.json");

        let s = Settings {
            server_url: "https://x.example".into(),
            ..Settings::default()
        };
        s.save(&path).expect("save should create parent dirs");
        assert!(path.exists());

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn settings_save_overwrites_existing_file() {
        let path = temp_settings_path("overwrite");
        cleanup(&path);

        Settings {
            server_url: "https://first.example".into(),
            autolaunch: false,
            ..Settings::default()
        }
        .save(&path)
        .unwrap();

        let updated = Settings {
            server_url: "https://second.example".into(),
            autolaunch: true,
            device_id: "newdev".into(),
            ..Settings::default()
        };
        updated.save(&path).unwrap();

        let loaded = Settings::load(&path);
        assert_eq!(loaded, updated, "second save must fully replace the first");

        cleanup(&path);
    }

    // --- path helper --------------------------------------------------------

    #[test]
    fn settings_path_in_appends_file_name() {
        let p = Settings::path_in(Path::new("/var/lib/pushfree"));
        assert_eq!(p.file_name().unwrap(), FILE_NAME);
    }
}
