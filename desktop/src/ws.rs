//! Direct WebSocket client for the pushfree Open Client protocol.
//!
//! Connects with `tokio-tungstenite` directly — NOT via a Tauri HTTP/WS plugin
//! — per the EB/A3 decision: a plugin's IPC layer swallows the transport-level
//! signals (read timeouts, close codes, write back-pressure) that this client
//! must observe to honour the reconnect/backoff contract faithfully.
//!
//! # Protocol (server `internal/hub`, todo 13)
//! `GET /1/ws?since=<id>` upgrades to WebSocket. The first text message the
//! client sends is the login line:
//!
//! ```text
//! {"type":"login","device_id":"...","secret":"..."}\n
//! ```
//!
//! On success the server replies `{"type":"open","last_message_id":N}`, then
//! streams `{"type":"message",...}` frames (JSON, one per WS text message) and
//! a `{"type":"keepalive"}` frame every 45s. Auth failure closes the socket
//! with application close code 4001.
//!
//! # Timing (research EB/W2-ws.md)
//! - read timeout: 77s (server keepalive 45s + a comfortable margin). If no
//!   frame arrives within this window the connection is presumed dead and the
//!   client reconnects. This is asserted as a constant below.
//! - client ping interval: 30s (keeps NAT/proxies warm; complements the
//!   server's application-level keepalive).
//! - reconnect backoff: AWS "Full Jitter" — `delay = U * min(cap, base*2^n)`,
//!   base 1s, cap 60s. `U` is uniform on `[0,1)`.
//!
//! # Cursor persistence (choice documented)
//! The since cursor (highest message id observed) is persisted to a single
//! plain-text file (atomic temp-write + rename) when `WsConfig::cursor_path`
//! is `Some`. A plain file is the simplest durable store that needs no extra
//! crate, schema, or async I/O; per-message writes are sub-millisecond and
//! acceptable for a low-volume desktop notifier. todo 39 points this at the
//! app-data directory. When `None`, the cursor lives only in memory.

use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use futures_util::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};

use tokio::sync::mpsc;
use tokio::time::Instant as TokioInstant;
use tokio_tungstenite::connect_async;
use tokio_tungstenite::tungstenite::Message;

// ---------------------------------------------------------------------------
// Timing constants
// ---------------------------------------------------------------------------

/// Server application keepalive interval (informational). The client tolerates
/// it and uses [`READ_TIMEOUT`] to detect a server that has stopped talking.
pub const SERVER_KEEPALIVE: Duration = Duration::from_secs(45);

/// Production read timeout. Must stay at 77s -- the server keepalive (45s)
/// plus a comfortable margin -- so a healthy connection never trips it. 77s
/// is the floor documented in EB/W2-ws.md; asserted in tests.
pub const READ_TIMEOUT: Duration = Duration::from_secs(77);

/// Client-to-server WS Ping interval. Complements the server's application
/// keepalive and keeps intermediary NAT/proxies from dropping the TCP flow.
pub const PING_INTERVAL: Duration = Duration::from_secs(30);

/// Full-Jitter backoff base (AWS "Exponential Backoff With Jitter").
pub const BACKOFF_BASE: Duration = Duration::from_secs(1);
/// Full-Jitter backoff cap.
pub const BACKOFF_CAP: Duration = Duration::from_secs(60);

// ---------------------------------------------------------------------------
// Full-Jitter backoff (pure, no I/O, no sleeps)
// ---------------------------------------------------------------------------

/// Full-Jitter backoff *ceiling* for `attempt` (0-indexed):
/// `min(cap, base * 2^attempt)`. This is the upper bound before a uniform
/// jitter factor is applied. Pure/deterministic so it can be asserted without
/// any sleeps.
pub fn backoff_ceiling(attempt: u32, base: Duration, cap: Duration) -> Duration {
    let cap_ms = cap.as_millis();
    let mut ms: u128 = base.as_millis();
    for _ in 0..attempt {
        if ms >= cap_ms {
            break;
        }
        ms = ms.saturating_mul(2);
    }
    let ms = ms.min(cap_ms);
    Duration::from_millis(u64::try_from(ms).unwrap_or(u64::MAX))
}

/// Full-Jitter delay for `attempt`, given a uniform factor `unit` in `[0,1]`:
/// `round(unit * ceiling)`. `unit` is clamped, so callers cannot overshoot.
pub fn backoff_delay(attempt: u32, base: Duration, cap: Duration, unit: f64) -> Duration {
    let ceil = backoff_ceiling(attempt, base, cap);
    let unit = unit.clamp(0.0, 1.0);
    let ms = (ceil.as_millis() as f64 * unit).round() as u64;
    Duration::from_millis(ms)
}

/// Production jitter source: a seedable xorshift32 PRNG yielding uniform
/// factors on `[0,1)`. Seeded from wall-clock nanos; not cryptographic, which
/// is fine — jitter only needs uniformity to decorrelate reconnect storms.
pub struct Jitter {
    state: Mutex<u32>,
}

impl Jitter {
    /// Construct a jitter source seeded from the wall clock.
    pub fn new() -> Self {
        // | 1 guarantees a non-zero state (xorshift32 cannot escape 0).
        let seed = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.subsec_nanos())
            .unwrap_or(0xDEAD_BEEF)
            | 1;
        Jitter {
            state: Mutex::new(seed),
        }
    }

    /// The next reconnect delay for `attempt` under the configured bounds.
    pub fn delay(&self, attempt: u32, base: Duration, cap: Duration) -> Duration {
        backoff_delay(attempt, base, cap, self.next_unit())
    }

    fn next_unit(&self) -> f64 {
        let mut state = self.state.lock().expect("jitter state poisoned");
        let mut x = *state;
        // xorshift32 (Marsaglia).
        x ^= x << 13;
        x ^= x >> 17;
        x ^= x << 5;
        *state = x;
        // Map the full u32 range onto [0, 1).
        (x as f64) / ((u32::MAX as f64) + 1.0)
    }
}

impl Default for Jitter {
    fn default() -> Self {
        Self::new()
    }
}

// ---------------------------------------------------------------------------
// Since-cursor persistence
// ---------------------------------------------------------------------------

/// The since-cursor: the highest message id the client has observed. On
/// reconnect the client passes this as `?since=` so the server replays only
/// newer messages.
pub struct Cursor {
    current: Mutex<i64>,
    path: Option<PathBuf>,
}

impl Cursor {
    /// Load the cursor, reading any persisted value from `path`. A missing or
    /// malformed file yields `0` (replay from the start).
    pub fn new(path: Option<PathBuf>) -> Self {
        let initial = path.as_ref().map(|p| Self::read(p)).unwrap_or(0);
        Cursor {
            current: Mutex::new(initial),
            path,
        }
    }

    /// Current high-water mark.
    pub fn load(&self) -> i64 {
        *self.current.lock().expect("cursor lock poisoned")
    }

    /// Record observation of `id`; persists if `id` advances the high-water
    /// mark and a path is configured.
    pub fn observe(&self, id: i64) {
        let mut cur = self.current.lock().expect("cursor lock poisoned");
        if id > *cur {
            *cur = id;
            if let Some(path) = &self.path {
                Self::write(path, id);
            }
        }
    }

    fn read(path: &Path) -> i64 {
        match std::fs::read_to_string(path) {
            Ok(s) => s.trim().parse::<i64>().unwrap_or(0),
            Err(_) => 0,
        }
    }

    /// Atomic write: create `<name>.tmp` then rename over the target. The
    /// rename is atomic on the same filesystem (POSIX rename / Win32
    /// MoveFileEx-with-replace), so a crash mid-write never leaves a truncated
    /// cursor — at worst the previous value remains.
    fn write(path: &Path, id: i64) {
        use std::io::Write;
        if let Some(parent) = path.parent() {
            if !parent.as_os_str().is_empty() {
                let _ = std::fs::create_dir_all(parent);
            }
        }
        let tmp = path.with_file_name(format!(
            "{}.tmp",
            path.file_name()
                .and_then(|n| n.to_str())
                .unwrap_or("cursor")
        ));
        let ok = std::fs::File::create(&tmp)
            .and_then(|mut f| writeln!(f, "{id}").and_then(|()| f.sync_all()))
            .is_ok();
        if ok {
            let _ = std::fs::rename(&tmp, path);
        }
    }
}

// ---------------------------------------------------------------------------
// Frames
// ---------------------------------------------------------------------------

/// Build the absolute WebSocket URL `GET /1/ws?since=<id>` from a server base.
/// A trailing slash on the base is tolerated.
pub fn build_ws_url(server_base_url: &str, since: i64) -> String {
    let base = server_base_url.trim_end_matches('/');
    format!("{base}/1/ws?since={since}")
}

/// A decoded `{"type":"message",...}` server frame. Field names mirror the
/// server's `StoredMessage` JSON (todo 13); unknown fields are ignored so a
/// newer server never breaks an older client.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct ServerMessage {
    pub id: i64,
    pub send_id: i64,
    pub priority: i32,
    #[serde(default)]
    pub sound: String,
    #[serde(default)]
    pub title: String,
    pub message: String,
    #[serde(default)]
    pub url: String,
    #[serde(default)]
    pub url_title: String,
    #[serde(default)]
    pub html: bool,
    #[serde(default)]
    pub monospace: bool,
    pub timestamp: i64,
    #[serde(default)]
    pub ttl: i64,
    #[serde(default)]
    pub tag: String,
    #[serde(default)]
    pub encrypted: bool,
}

/// The login line sent as the first WS text frame.
#[derive(Serialize)]
struct LoginFrame<'a> {
    #[serde(rename = "type")]
    typ: &'a str,
    device_id: &'a str,
    secret: &'a str,
}

/// Incoming server frames, tagged on `type` (serde internally-tagged).
#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum ServerFrame {
    Open { last_message_id: i64 },
    Message(ServerMessage),
    Keepalive,
}

// ---------------------------------------------------------------------------
// Configuration & client
// ---------------------------------------------------------------------------

/// A configuration or connection error from [`Client::new`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ConfigError {
    /// `server_url` did not start with `ws://` or `wss://`.
    InvalidUrl,
    /// `device_id` or `secret` was empty.
    MissingCredentials,
    /// `backoff_cap` was smaller than `backoff_base`.
    BackoffCapBelowBase,
    /// A timing field (`read_timeout` or `ping_interval`) was zero.
    InvalidTiming,
}

impl std::fmt::Display for ConfigError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConfigError::InvalidUrl => write!(f, "server_url must start with ws:// or wss://"),
            ConfigError::MissingCredentials => write!(f, "device_id and secret are required"),
            ConfigError::BackoffCapBelowBase => write!(f, "backoff_cap must be >= backoff_base"),
            ConfigError::InvalidTiming => write!(f, "read_timeout and ping_interval must be > 0"),
        }
    }
}

impl std::error::Error for ConfigError {}

/// All knobs for the WS client. Fields are public so callers (todo 39) and
/// tests override individual values; defaults come from [`WsConfig::new`].
#[derive(Clone, Debug)]
pub struct WsConfig {
    pub server_url: String,
    pub device_id: String,
    pub secret: String,
    pub read_timeout: Duration,
    pub ping_interval: Duration,
    pub backoff_base: Duration,
    pub backoff_cap: Duration,
    pub cursor_path: Option<PathBuf>,
}

impl WsConfig {
    /// Build a config with production defaults: 77s read timeout, 30s ping,
    /// Full-Jitter 1s/60s, no persisted cursor (todo 39 sets the path).
    pub fn new(server_url: String, device_id: String, secret: String) -> Self {
        WsConfig {
            server_url,
            device_id,
            secret,
            read_timeout: READ_TIMEOUT,
            ping_interval: PING_INTERVAL,
            backoff_base: BACKOFF_BASE,
            backoff_cap: BACKOFF_CAP,
            cursor_path: None,
        }
    }
}

/// Observable client events. Consumers (todo 38 notification pipeline) match
/// on these to surface connection status and decoded messages.
#[derive(Debug, Clone)]
pub enum Event {
    /// About to open a fresh connection on this (0-indexed) attempt.
    Connecting { attempt: u32 },
    /// The WebSocket upgrade succeeded (before the login handshake).
    Connected,
    /// The server accepted the login; `last_message_id` is its high-water mark.
    Open { last_message_id: i64 },
    /// A decoded message frame. The cursor has already been advanced to `id`.
    Message(ServerMessage),
    /// A keepalive frame was received.
    Keepalive,
    /// A previously-open connection ended. `close_code` is the WS close code
    /// when one was signalled (e.g. 4001 on auth failure).
    Disconnected {
        attempt: u32,
        close_code: Option<u16>,
    },
    /// No frame arrived within `read_timeout`; treating the link as dead.
    ReadTimeout { attempt: u32 },
    /// Sleeping for `delay` before the next attempt (Full-Jitter).
    BackoffScheduled { attempt: u32, delay: Duration },
    /// The `shutdown` signal fired; the run loop is exiting.
    Shutdown,
}

/// The result of a single connection attempt's lifetime.
enum Outcome {
    /// The read timeout fired (silent server).
    ReadTimeout,
    /// The transport closed; `close_code` is the WS close code if present.
    Disconnected { close_code: Option<u16> },
}

/// Result of decoding one inbound frame.
enum FrameOutcome {
    Continue,
    Closed(u16),
}

/// The pushfree WebSocket client. Construct with [`Client::new`] and drive
/// with [`Client::run`].
pub struct Client {
    config: WsConfig,
    cursor: Cursor,
}

impl Client {
    /// Validate the config and build a client.
    pub fn new(config: WsConfig) -> Result<Self, ConfigError> {
        if !config.server_url.starts_with("ws://") && !config.server_url.starts_with("wss://") {
            return Err(ConfigError::InvalidUrl);
        }
        if config.device_id.is_empty() || config.secret.is_empty() {
            return Err(ConfigError::MissingCredentials);
        }
        if config.backoff_cap < config.backoff_base {
            return Err(ConfigError::BackoffCapBelowBase);
        }
        if config.read_timeout.is_zero() || config.ping_interval.is_zero() {
            return Err(ConfigError::InvalidTiming);
        }
        let cursor = Cursor::new(config.cursor_path.clone());
        Ok(Client { config, cursor })
    }

    /// Run the connect -> handshake -> stream -> backoff -> reconnect loop
    /// until `shutdown` resolves (its output is ignored). Events flow out on
    /// `tx`. Designed to be driven inside a spawned task holding the client
    /// behind an `Arc`.
    pub async fn run<F>(&self, tx: mpsc::Sender<Event>, shutdown: F)
    where
        F: std::future::Future + Send,
    {
        tokio::pin!(shutdown);
        let jitter = Jitter::new();
        let mut attempt: u32 = 0;

        loop {
            let _ = tx.send(Event::Connecting { attempt }).await;
            let outcome = tokio::select! {
                _ = &mut shutdown => {
                    let _ = tx.send(Event::Shutdown).await;
                    return;
                }
                o = self.connect_once(tx.clone()) => o,
            };

            let delay = jitter.delay(attempt, self.config.backoff_base, self.config.backoff_cap);
            match outcome {
                Outcome::ReadTimeout => {
                    let _ = tx.send(Event::ReadTimeout { attempt }).await;
                    let _ = tx.send(Event::BackoffScheduled { attempt, delay }).await;
                }
                Outcome::Disconnected { close_code } => {
                    let _ = tx
                        .send(Event::Disconnected {
                            attempt,
                            close_code,
                        })
                        .await;
                    let _ = tx.send(Event::BackoffScheduled { attempt, delay }).await;
                }
            }

            tokio::select! {
                _ = &mut shutdown => {
                    let _ = tx.send(Event::Shutdown).await;
                    return;
                }
                _ = tokio::time::sleep(delay) => {}
            }
            attempt = attempt.saturating_add(1);
        }
    }

    /// One full connection lifetime: upgrade → login handshake → stream until
    /// the transport ends or the read timeout fires.
    async fn connect_once(&self, tx: mpsc::Sender<Event>) -> Outcome {
        let url = build_ws_url(&self.config.server_url, self.cursor.load());
        let stream = match connect_async(url).await {
            Ok((s, _)) => s,
            Err(e) => {
                eprintln!("[pushfree/ws] connect failed: {e}");
                return Outcome::Disconnected { close_code: None };
            }
        };
        let _ = tx.send(Event::Connected).await;

        let (mut writer, mut reader) = stream.split();

        // 1. Login handshake: the first text frame is the JSON login line
        //    terminated by a newline, exactly as the server contract requires.
        let login = serde_json::to_string(&LoginFrame {
            typ: "login",
            device_id: &self.config.device_id,
            secret: &self.config.secret,
        })
        .expect("login frame is always serializable");
        if writer
            .send(Message::Text(format!("{login}\n").into()))
            .await
            .is_err()
        {
            return Outcome::Disconnected { close_code: None };
        }

        // 2. Drive the read loop with a periodic ping and a read-timeout
        //    watchdog. `interval`'s first tick is immediate; consume it so the
        //    first real ping lands one full interval after connect.
        let mut ping = tokio::time::interval(self.config.ping_interval);
        ping.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        ping.tick().await;

        let mut last_recv = TokioInstant::now();
        loop {
            let deadline = last_recv + self.config.read_timeout;
            tokio::select! {
                biased;
                msg = reader.next() => {
                    last_recv = TokioInstant::now();
                    match msg {
                        None => return Outcome::Disconnected { close_code: None },
                        Some(Err(e)) => {
                            eprintln!("[pushfree/ws] read error: {e}");
                            return Outcome::Disconnected { close_code: None };
                        }
                        Some(Ok(m)) => {
                            if let FrameOutcome::Closed(code) =
                                self.handle_frame(m, &tx).await
                            {
                                return Outcome::Disconnected { close_code: Some(code) };
                            }
                        }
                    }
                }
                _ = ping.tick() => {
                    if writer.send(Message::Ping(vec![0x70_u8].into())).await.is_err() {
                        return Outcome::Disconnected { close_code: None };
                    }
                }
                _ = tokio::time::sleep_until(deadline) => {
                    return Outcome::ReadTimeout;
                }
            }
        }
    }

    /// Decode one inbound frame. Text frames carry JSON; close frames end the
    /// connection; everything else (binary, raw ping/pong, frame) is ignored
    /// (tungstenite handles pong replies automatically).
    async fn handle_frame(&self, msg: Message, tx: &mpsc::Sender<Event>) -> FrameOutcome {
        match msg {
            Message::Text(text) => match serde_json::from_str::<ServerFrame>(text.as_str()) {
                Ok(ServerFrame::Open { last_message_id }) => {
                    let _ = tx.send(Event::Open { last_message_id }).await;
                    FrameOutcome::Continue
                }
                Ok(ServerFrame::Message(m)) => {
                    self.cursor.observe(m.id);
                    let _ = tx.send(Event::Message(m)).await;
                    FrameOutcome::Continue
                }
                Ok(ServerFrame::Keepalive) => {
                    let _ = tx.send(Event::Keepalive).await;
                    FrameOutcome::Continue
                }
                Err(e) => {
                    eprintln!("[pushfree/ws] dropping malformed frame ({e})");
                    FrameOutcome::Continue
                }
            },
            Message::Close(frame) => {
                FrameOutcome::Closed(frame.map(|f| u16::from(f.code)).unwrap_or(1000))
            }
            _ => FrameOutcome::Continue,
        }
    }
}

#[cfg(test)]
mod tests;
