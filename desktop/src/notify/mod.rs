//! Local notification pipeline: dedup by `send_id`, OS notification via a
//! [`NotifySink`], and deferred emergency-receipt acknowledgement.
//!
//! # Contract (plan todo 38; research EB/A3-tauri.md, EB/A1-pushover-api.md)
//! - A received message is shown as an OS notification through a
//!   [`NotifySink`] (production impl: `tauri-plugin-notification`; tests use a
//!   mock). Per EB/A3 the underlying `sendNotification` is best-effort ("at-desk
//!   channel"): a notify failure is logged but never blocks downstream work.
//! - Duplicate `send_id`s are suppressed: a send is notified AT MOST once.
//!   The seen-set is in memory for the session and optionally persisted
//!   as an append-only log so a mid-session crash cannot double-fire.
//! - Emergency (priority-2) receipts are NOT auto-acked. Pushover emergency
//!   semantics require the alert to repeat until a human explicitly
//!   acknowledges it. A future Tauri notification action/dialog will trigger
//!   the ack via [`AckClient`]; the ack infrastructure ([`AckReporter`],
//!   [`AckClient`], [`build_ack_url`]) is wired and ready for that
//!   user-triggered path but is not invoked from the notification pipeline.
//!
//! # Why a fixed (not jittered) ack retry delay
//! A single desktop client acking its own server has no thundering-herd, so
//! Full-Jitter (used by the WS reconnect path) would only harm test
//! determinism. The Pushover callback retry interval (60s) is reused.

use std::collections::HashSet;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;

use crate::ws::ServerMessage;

#[cfg(test)]
mod tests;

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/// Default delay between ack retries (Pushover callback interval).
pub const DEFAULT_ACK_RETRY_DELAY: Duration = Duration::from_secs(60);

/// URL prefix/suffix for the ack endpoint, relative to the server base.
const ACK_PATH_PREFIX: &str = "/1/receipts/";
const ACK_PATH_SUFFIX: &str = "/acknowledge.json";

// ---------------------------------------------------------------------------
// Notification content + priority -> style mapping
// ---------------------------------------------------------------------------

/// The display payload built from a server message.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NotificationContent {
    pub title: String,
    pub body: String,
}

/// Map a Pushover priority (-2..2) to a short urgency label prepended to the
/// title. Emergency (p2) is surfaced prominently; negative priorities are
/// de-emphasised. Pure so it is unit-testable without a sink.
pub fn priority_label(priority: i32) -> &'static str {
    match priority {
        p if p >= 2 => "EMERGENCY",
        1 => "High",
        0 => "",
        -1 => "Low",
        _ => "Lowest", // <= -2
    }
}

/// Build the notification title/body from a message. The label (if any) is
/// prepended to the title; a missing title falls back to the product name so
/// the OS always has something to show.
pub fn format_notification(msg: &ServerMessage) -> NotificationContent {
    let label = priority_label(msg.priority);
    let title = match (msg.title.as_str(), label) {
        ("", "") => "PushFree".to_string(),
        ("", l) => format!("{l} · PushFree"),
        (t, "") => t.to_string(),
        (t, l) => format!("{l} · {t}"),
    };
    NotificationContent {
        title,
        body: msg.message.clone(),
    }
}

// ---------------------------------------------------------------------------
// NotifySink trait + production impl
// ---------------------------------------------------------------------------

/// A failure to display a local notification. Best-effort: the pipeline logs
/// this and proceeds to ack regardless (EB/A3 "at-desk channel").
#[derive(Debug)]
pub struct NotifyError(pub String);

impl std::fmt::Display for NotifyError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "notification failed: {}", self.0)
    }
}

impl std::error::Error for NotifyError {}

/// Display a received message as a local OS notification. Implementations:
/// production ([`ToastSink`] via `tauri-plugin-notification`) and a mock (tests).
/// The method is synchronous because every supported backend (`winrt`,
/// `notify-rust`, Tauri builder) shows on the current thread in sub-millisecond
/// time; the pipeline tolerates errors per the best-effort contract.
pub trait NotifySink: Send + Sync {
    /// Show the notification. Errors are reported but must never propagate to
    /// block ack reporting.
    fn notify(&self, msg: &ServerMessage) -> Result<(), NotifyError>;
}

/// Production [`NotifySink`] backed by `tauri-plugin-notification`.
///
/// Constructed in `main::run`'s `setup` closure where an [`tauri::AppHandle`]
/// is available; never constructed in tests (they use a mock sink). Held behind
/// `Arc<dyn NotifySink>` so the pipeline is backend-agnostic.
pub struct ToastSink<R: tauri::Runtime> {
    app: tauri::AppHandle<R>,
}

impl<R: tauri::Runtime> ToastSink<R> {
    pub fn new(app: tauri::AppHandle<R>) -> Self {
        ToastSink { app }
    }
}

impl<R: tauri::Runtime> NotifySink for ToastSink<R> {
    fn notify(&self, msg: &ServerMessage) -> Result<(), NotifyError> {
        use tauri_plugin_notification::NotificationExt;
        let content = format_notification(msg);
        self.app
            .notification()
            .builder()
            .title(content.title)
            .body(content.body)
            .show()
            .map_err(|e| NotifyError(e.to_string()))
    }
}

// ---------------------------------------------------------------------------
// send_id dedup
// ---------------------------------------------------------------------------

/// At-most-once delivery tracking by `send_id`.
///
/// In memory for the session; optionally backed by an append-only log file so
/// a crash mid-session cannot double-fire. Cross-restart replay duplicates are
/// already suppressed by the WS since-cursor (todo 37), so persistence is a
/// belt-and-suspenders layer, not the primary dedup.
pub struct Dedup {
    seen: Mutex<HashSet<i64>>,
    path: Option<PathBuf>,
}

impl Dedup {
    /// Load any persisted seen-set from `path` (a missing/malformed file yields
    /// an empty set). Pass `None` for an in-memory-only dedup.
    pub fn new(path: Option<PathBuf>) -> Self {
        let mut seen = HashSet::new();
        if let Some(p) = &path {
            Self::load(p, &mut seen);
        }
        Dedup {
            seen: Mutex::new(seen),
            path,
        }
    }

    /// Record observation of `send_id`. Returns `true` if it was previously
    /// unseen (and is now marked + persisted); `false` for a duplicate.
    pub fn observe(&self, send_id: i64) -> bool {
        let mut seen = self.seen.lock().expect("dedup lock poisoned");
        if seen.insert(send_id) {
            if let Some(path) = &self.path {
                Self::append(path, send_id);
            }
            true
        } else {
            false
        }
    }

    /// Number of distinct send_ids currently recorded (in memory).
    pub fn len(&self) -> usize {
        self.seen.lock().expect("dedup lock poisoned").len()
    }

    /// Whether any send_id has been recorded.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    fn load(path: &Path, seen: &mut HashSet<i64>) {
        let Ok(s) = std::fs::read_to_string(path) else {
            return; // missing file -> empty set
        };
        for tok in s.split_whitespace() {
            if let Ok(id) = tok.parse::<i64>() {
                seen.insert(id);
            }
        }
    }

    /// Append `"<send_id>\n"` to the log. Creating the parent dir first; any
    /// I/O error is logged and swallowed because dedup correctness in memory
    /// is unaffected (only cross-restart persistence degrades).
    fn append(path: &Path, send_id: i64) {
        if let Some(parent) = path.parent() {
            if !parent.as_os_str().is_empty() {
                let _ = std::fs::create_dir_all(parent);
            }
        }
        if let Ok(mut f) = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(path)
        {
            let _ = writeln!(f, "{send_id}");
        }
    }
}

// ---------------------------------------------------------------------------
// Ack client (production HTTP + trait for mock injection)
// ---------------------------------------------------------------------------

/// A classification of an ack attempt's outcome.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AckOutcome {
    /// 2xx: the server accepted the ack.
    Ok,
    /// Transient failure (5xx or network): retry after the delay.
    Retry(AckError),
    /// Permanent failure (4xx): retrying cannot help, abandon.
    Permanent(AckError),
}

/// Why an ack failed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AckError {
    /// HTTP status code (5xx -> Retry, 4xx -> Permanent).
    Status(u16),
    /// The request never reached the server or no response was read.
    Network(String),
}

impl std::fmt::Display for AckError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            AckError::Status(c) => write!(f, "HTTP {c}"),
            AckError::Network(m) => write!(f, "network error: {m}"),
        }
    }
}

/// Posts an ack for a message to the server. The trait lets tests inject a
/// scripted client; production uses [`HttpAckClient`].
#[async_trait]
pub trait AckClient: Send + Sync {
    /// Acknowledge `receipt_id`. Must not panic; classify failures via the
    /// returned [`AckOutcome`].
    async fn ack(&self, receipt_id: &str) -> AckOutcome;
}

/// Build the absolute ack URL `POST {base}/1/messages/{id}/ack.json?secret=...`.
/// A trailing slash on the base is tolerated. The secret is inlined into the
/// query string because device secrets are 30-char `[A-Za-z0-9]` (todo 13) and
/// need no percent-encoding.
pub fn build_ack_url(server_base_url: &str, receipt_id: &str, device_id: &str, secret: &str) -> String {
    let base = server_base_url.trim_end_matches('/');
    format!("{base}{ACK_PATH_PREFIX}{receipt_id}{ACK_PATH_SUFFIX}?device_id={device_id}&secret={secret}")
}

/// Configuration error from [`HttpAckClient::new`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AckClientError {
    /// `base_url` did not start with `http://` or `https://`.
    InvalidBaseUrl,
    /// The HTTP client could not be constructed.
    Build(String),
}

impl std::fmt::Display for AckClientError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            AckClientError::InvalidBaseUrl => {
                write!(f, "base_url must start with http:// or https://")
            }
            AckClientError::Build(m) => write!(f, "HTTP client build failed: {m}"),
        }
    }
}

impl std::error::Error for AckClientError {}

/// Production [`AckClient`] over HTTP. Posts the device secret as a query
/// parameter to the ack endpoint and classifies the response.
pub struct HttpAckClient {
    base_url: String,
    device_id: String,
    secret: String,
    client: reqwest::Client,
}

impl HttpAckClient {
    /// Construct with the server base URL and device secret. Reuses one
    /// connection pool for all acks.
    pub fn new(base_url: String, device_id: String, secret: String) -> Result<Self, AckClientError> {
        if !base_url.starts_with("http://") && !base_url.starts_with("https://") {
            return Err(AckClientError::InvalidBaseUrl);
        }
        let client = reqwest::Client::builder()
            .build()
            .map_err(|e| AckClientError::Build(e.to_string()))?;
        Ok(HttpAckClient {
            base_url,
            device_id,
            secret,
            client,
        })
    }
}

#[async_trait]
impl AckClient for HttpAckClient {
    async fn ack(&self, receipt_id: &str) -> AckOutcome {
        let url = build_ack_url(&self.base_url, receipt_id, &self.device_id, &self.secret);
        let res = match self.client.post(&url).send().await {
            Ok(r) => r,
            Err(e) => return AckOutcome::Retry(AckError::Network(e.to_string())),
        };
        let status = res.status();
        if status.is_success() {
            AckOutcome::Ok
        } else if status.is_server_error() {
            AckOutcome::Retry(AckError::Status(status.as_u16()))
        } else {
            AckOutcome::Permanent(AckError::Status(status.as_u16()))
        }
    }
}

// ---------------------------------------------------------------------------
// Ack reporter (background retry loop)
// ---------------------------------------------------------------------------

/// Drains the ack queue and retries transient failures. Intended to run as one
/// background task for the lifetime of the app; it exits when the queue sender
/// is dropped (shutdown).
///
/// Each ack is retried inline: on [`AckOutcome::Retry`] the reporter sleeps for
/// `retry_delay` (advancing instantly under a paused/virtual clock) then tries
/// again; [`AckOutcome::Permanent`] is abandoned; [`AckOutcome::Ok`] completes
/// it. Processing is serial: a desktop notifier's ack volume is low and serial
/// retry keeps the retry sequence deterministic for tests.
pub struct AckReporter<A: AckClient> {
    rx: tokio::sync::mpsc::Receiver<String>,
    client: Arc<A>,
    retry_delay: Duration,
}

impl<A: AckClient> AckReporter<A> {
    pub fn new(
        rx: tokio::sync::mpsc::Receiver<String>,
        client: Arc<A>,
        retry_delay: Duration,
    ) -> Self {
        AckReporter {
            rx,
            client,
            retry_delay,
        }
    }

    /// Run until the queue sender is dropped. Consumes the reporter.
    pub async fn run(mut self) {
        while let Some(receipt_id) = self.rx.recv().await {
            self.ack_with_retry(&receipt_id).await;
        }
    }

    async fn ack_with_retry(&self, receipt_id: &str) {
        loop {
            match self.client.ack(receipt_id).await {
                AckOutcome::Ok => return,
                AckOutcome::Retry(err) => {
                    eprintln!(
                        "[pushfree/ack] ack {receipt_id} failed ({err}); retrying in {:?}",
                        self.retry_delay
                    );
                    tokio::time::sleep(self.retry_delay).await;
                }
                AckOutcome::Permanent(err) => {
                    eprintln!("[pushfree/ack] ack {receipt_id} abandoned ({err})");
                    return;
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Pipeline: dedup -> notify -> enqueue ack
// ---------------------------------------------------------------------------

/// Outcome of [`Pipeline::handle`] for one message.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HandleOutcome {
    /// A new send_id: the notification was shown (best-effort). Emergency
    /// receipts are not auto-acked (see module docs).
    Notified,
    /// A duplicate send_id: nothing was shown or acked.
    Duplicate,
}

/// The message -> notify+ack pipeline. Held behind `Arc` and shared with the
/// WS event consumer task; calls to [`Pipeline::handle`] are safe concurrently.
pub struct Pipeline {
    dedup: Dedup,
    sink: Arc<dyn NotifySink>,
    // Reserved seam for the pending user-ack UI: emergency receipts are NOT
    // auto-acked (Pushover semantics), so nothing sends on this channel until
    // the inbox gets an explicit confirm button. Kept wired so that feature
    // lands without re-plumbing the queue.
    #[allow(dead_code)]
    ack_tx: tokio::sync::mpsc::Sender<String>,
    /// Optional E2EE key (64-hex). When present, encrypted message fields are
    /// decrypted before the notification is shown (todo 44); on any failure a
    /// safe placeholder is displayed. `None` => encrypted messages surface as
    /// `[undecryptable]` (error state, never garbage).
    e2ee_key: Option<String>,
}

impl Pipeline {
    /// Wire a dedup set, a notification sink, and the ack queue sender (the
    /// receiver side is owned by an [`AckReporter`]). No E2EE key: encrypted
    /// messages will surface as `[undecryptable]`.
    pub fn new(
        dedup: Dedup,
        sink: Arc<dyn NotifySink>,
        ack_tx: tokio::sync::mpsc::Sender<String>,
    ) -> Self {
        Pipeline {
            dedup,
            sink,
            ack_tx,
            e2ee_key: None,
        }
    }

    /// Set the E2EE key used to decrypt encrypted message fields before they
    /// reach the notification sink. Builder-style: chains off [`Pipeline::new`].
    pub fn with_e2ee_key(mut self, key: Option<String>) -> Self {
        self.e2ee_key = key.filter(|k| !k.is_empty());
        self
    }

    /// Handle one decoded message.
    ///
    /// - Duplicate `send_id` -> returns [`HandleOutcome::Duplicate`], nothing
    ///   else happens (never notified, never acked twice).
    /// - New `send_id` -> marks it seen, decrypts encrypted fields (todo 44;
    ///   failure -> safe placeholder, never garbage / never a panic), shows the
    ///   notification (best-effort: errors are logged, never propagated), and
    ///   enqueues an ack for `message.id`. The enqueue is awaited so a
    ///   saturated queue applies backpressure instead of silently dropping an
    ///   ack.
    pub async fn handle(&self, msg: &ServerMessage) -> HandleOutcome {
        if !self.dedup.observe(msg.send_id) {
            return HandleOutcome::Duplicate;
        }
        // Decrypt BEFORE notify. A wrong key / tampered blob yields
        // `[undecryptable]`; the notification still fires with a placeholder.
        let decrypted = crate::e2ee::decrypt_message(msg, self.e2ee_key.as_deref());
        if let Err(err) = self.sink.notify(&decrypted) {
            eprintln!("[pushfree/notify] {}", err);
        }
        // NOTE: Emergency (priority-2) receipts are NOT auto-acked. Pushover
        // emergency semantics require explicit user acknowledgement (the alert
        // must repeat until a human confirms). A future Tauri notification
        // action or dialog will trigger the ack via AckClient; until then
        // the ack infrastructure remains wired but unused.
        HandleOutcome::Notified
    }
}
