//! Tests for the WS client.
//!
//! Backoff math is exercised by pure unit tests (no sleeps). The wire
//! behaviour — handshake, since-replay, reconnect-after-close-4001, read
//! timeout — is exercised against an in-process tokio-tungstenite mock server.
//! Each test guards itself with a hard real-time deadline so a regression
//! fails fast instead of hanging.

use super::*;

use std::net::SocketAddr;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;

use tokio::net::TcpListener;
use tokio::sync::{mpsc, oneshot};
use tokio::task::JoinHandle;
use tokio_tungstenite::accept_async;
use tokio_tungstenite::tungstenite::protocol::frame::coding::CloseCode;
use tokio_tungstenite::tungstenite::protocol::CloseFrame;
use tokio_tungstenite::WebSocketStream;

use tokio::net::TcpStream;

// ---------------------------------------------------------------------------
// Mock server
// ---------------------------------------------------------------------------

/// RAII guard for the mock server's accept-loop task. Aborting on drop
/// (including via a failed assertion) frees the bound port and stops accepting.
struct MockServer {
    handle: JoinHandle<()>,
}

impl Drop for MockServer {
    fn drop(&mut self) {
        self.handle.abort();
    }
}

/// Start a mock WS server on an ephemeral port. Each accepted connection is
/// handed to `handler` along with its 0-indexed connection number.
async fn start_mock<F, Fut>(handler: F) -> (SocketAddr, MockServer)
where
    F: Fn(u32, WebSocketStream<TcpStream>) -> Fut + Send + Sync + 'static,
    Fut: std::future::Future<Output = ()> + Send + 'static,
{
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let counter = Arc::new(AtomicU32::new(0));
    let handler = Arc::new(handler);
    let handle = tokio::spawn(async move {
        loop {
            let (stream, _) = match listener.accept().await {
                Ok(s) => s,
                Err(_) => break,
            };
            let ws = match accept_async(stream).await {
                Ok(w) => w,
                Err(_) => continue,
            };
            let n = counter.fetch_add(1, Ordering::SeqCst);
            let h = handler.clone();
            tokio::spawn(async move {
                h(n, ws).await;
            });
        }
    });
    (addr, MockServer { handle })
}

/// A unique cursor path under the temp dir so parallel tests never collide.
fn unique_tmp_path() -> PathBuf {
    static SEQ: AtomicU32 = AtomicU32::new(0);
    let n = SEQ.fetch_add(1, Ordering::SeqCst);
    let pid = std::process::id();
    std::env::temp_dir().join(format!("pushfree-ws-test-{pid}-{n}.cursor"))
}

const TEST_DEADLINE: Duration = Duration::from_secs(5);

// ---------------------------------------------------------------------------
// Pure unit tests: timing constants & backoff math
// ---------------------------------------------------------------------------

#[test]
fn read_timeout_meets_floor() {
    // Acceptance: read timeout must be >= 77s and clear the 45s keepalive.
    assert!(
        READ_TIMEOUT >= Duration::from_secs(77),
        "READ_TIMEOUT must be >= 77s, got {READ_TIMEOUT:?}"
    );
    assert!(
        READ_TIMEOUT > SERVER_KEEPALIVE,
        "read timeout must exceed server keepalive ({SERVER_KEEPALIVE:?})"
    );
    let margin = READ_TIMEOUT
        .checked_sub(SERVER_KEEPALIVE)
        .expect("read timeout > keepalive");
    assert!(
        margin >= Duration::from_secs(30),
        "keepalive margin must be >= 30s, got {margin:?}"
    );
    assert_eq!(PING_INTERVAL, Duration::from_secs(30));
    assert_eq!(BACKOFF_BASE, Duration::from_secs(1));
    assert_eq!(BACKOFF_CAP, Duration::from_secs(60));
}

#[test]
fn backoff_ceiling_doubles_then_caps() {
    let base = Duration::from_secs(1);
    let cap = Duration::from_secs(60);
    let seq: Vec<Duration> = (0..8).map(|a| backoff_ceiling(a, base, cap)).collect();
    // AWS Full-Jitter ceiling = min(cap, base * 2^attempt).
    assert_eq!(
        seq,
        vec![
            Duration::from_secs(1),
            Duration::from_secs(2),
            Duration::from_secs(4),
            Duration::from_secs(8),
            Duration::from_secs(16),
            Duration::from_secs(32),
            Duration::from_secs(60), // 64 would exceed cap -> capped
            Duration::from_secs(60), // stays at cap
        ]
    );
}

#[test]
fn backoff_ceiling_never_overflows() {
    // Absurd attempt values must saturate at the cap, not panic.
    let base = Duration::from_secs(1);
    let cap = Duration::from_secs(60);
    for a in [u32::MAX, 1000, 64] {
        assert_eq!(backoff_ceiling(a, base, cap), cap);
    }
    // Cap smaller than base -> ceiling is the cap.
    assert_eq!(
        backoff_ceiling(5, Duration::from_secs(10), Duration::from_secs(1)),
        Duration::from_secs(1)
    );
}

#[test]
fn backoff_full_jitter_bounds() {
    let base = Duration::from_secs(1);
    let cap = Duration::from_secs(60);
    for attempt in 0..12u32 {
        let ceil = backoff_ceiling(attempt, base, cap);
        // The sampled delay must lie in [0, ceiling] for every unit factor.
        for u in [0.0_f64, 0.1, 0.5, 0.9, 0.9999] {
            let d = backoff_delay(attempt, base, cap, u);
            assert!(
                d <= ceil,
                "attempt {attempt} unit {u}: delay {d:?} exceeds ceiling {ceil:?}"
            );
        }
        // unit=0 -> zero delay (the Full-Jitter lower bound).
        assert_eq!(backoff_delay(attempt, base, cap, 0.0), Duration::ZERO);
        // unit=1 -> exactly the ceiling.
        assert_eq!(backoff_delay(attempt, base, cap, 1.0), ceil);
    }
}

#[test]
fn backoff_unit_is_clamped() {
    // Overshoot / negative units must clamp, never overshoot the ceiling.
    let (base, cap) = (Duration::from_secs(1), Duration::from_secs(60));
    let ceil = backoff_ceiling(3, base, cap);
    assert_eq!(backoff_delay(3, base, cap, 5.0), ceil);
    assert_eq!(backoff_delay(3, base, cap, -1.0), Duration::ZERO);
}

#[test]
fn jitter_sampler_stays_within_bounds() {
    // The production RNG path: many samples must all lie in [0, ceiling].
    let jitter = Jitter::new();
    let base = Duration::from_secs(1);
    let cap = Duration::from_secs(60);
    for attempt in 0..12u32 {
        let ceil = backoff_ceiling(attempt, base, cap);
        for _ in 0..2000 {
            let d = jitter.delay(attempt, base, cap);
            assert!(
                d <= ceil,
                "sampled {d:?} > ceiling {ceil:?} (attempt {attempt})"
            );
        }
    }
}

// ---------------------------------------------------------------------------
// Pure unit tests: config, URL, cursor
// ---------------------------------------------------------------------------

#[test]
fn build_ws_url_appends_since_query() {
    assert_eq!(build_ws_url("ws://h:2586", 0), "ws://h:2586/1/ws?since=0");
    assert_eq!(
        build_ws_url("ws://h:2586/", 42),
        "ws://h:2586/1/ws?since=42"
    );
    assert_eq!(
        build_ws_url("wss://example.com", 999),
        "wss://example.com/1/ws?since=999"
    );
}

#[test]
fn client_config_validation() {
    assert!(Client::new(WsConfig::new("ws://h".into(), "d".into(), "s".into())).is_ok());

    assert!(matches!(
        Client::new(WsConfig::new("http://h".into(), "d".into(), "s".into())),
        Err(ConfigError::InvalidUrl)
    ));
    assert!(matches!(
        Client::new(WsConfig::new("ws://h".into(), "".into(), "s".into())),
        Err(ConfigError::MissingCredentials)
    ));
    assert!(matches!(
        Client::new(WsConfig::new("ws://h".into(), "d".into(), "".into())),
        Err(ConfigError::MissingCredentials)
    ));

    let mut cap_below_base = WsConfig::new("ws://h".into(), "d".into(), "s".into());
    cap_below_base.backoff_base = Duration::from_millis(2);
    cap_below_base.backoff_cap = Duration::from_millis(1);
    assert!(matches!(
        Client::new(cap_below_base),
        Err(ConfigError::BackoffCapBelowBase)
    ));

    let mut zero_timeout = WsConfig::new("ws://h".into(), "d".into(), "s".into());
    zero_timeout.read_timeout = Duration::ZERO;
    assert!(matches!(
        Client::new(zero_timeout),
        Err(ConfigError::InvalidTiming)
    ));
}

#[test]
fn cursor_file_roundtrip() {
    let path = unique_tmp_path();
    let c = Cursor::new(Some(path.clone()));
    assert_eq!(c.load(), 0, "missing file -> 0");

    c.observe(5);
    assert_eq!(c.load(), 5);
    c.observe(3); // older id must not regress the cursor
    assert_eq!(c.load(), 5);
    c.observe(10);
    assert_eq!(c.load(), 10);

    // Reload from disk to confirm persistence.
    let c2 = Cursor::new(Some(path.clone()));
    assert_eq!(c2.load(), 10);
    assert_eq!(
        std::fs::read_to_string(&path).unwrap().trim(),
        "10",
        "file holds the highest id"
    );

    let _ = std::fs::remove_file(&path);
}

#[test]
fn cursor_handles_corrupt_file() {
    let path = unique_tmp_path();
    std::fs::write(&path, "not-a-number").unwrap();
    let c = Cursor::new(Some(path.clone()));
    assert_eq!(c.load(), 0, "corrupt file -> 0 (replay from start)");
    let _ = std::fs::remove_file(&path);
}

#[test]
fn cursor_memory_when_no_path() {
    let c = Cursor::new(None);
    assert_eq!(c.load(), 0);
    c.observe(42);
    assert_eq!(c.load(), 42);
}

// ---------------------------------------------------------------------------
// Integration tests against the mock WS server
// ---------------------------------------------------------------------------

/// Spawn the client run loop on a dedicated task; returns the event receiver
/// and a shutdown sender. The caller owns `client` (kept alive via Arc).
fn spawn_client(
    client: Arc<Client>,
) -> (mpsc::Receiver<Event>, oneshot::Sender<()>, JoinHandle<()>) {
    let (tx, rx) = mpsc::channel(64);
    let (sd, rc) = oneshot::channel::<()>();
    let c = client;
    let task = tokio::spawn(async move {
        c.run(tx, rc).await;
    });
    (rx, sd, task)
}

async fn drain_until_open(rx: &mut mpsc::Receiver<Event>) -> Option<i64> {
    while let Some(ev) = rx.recv().await {
        if let Event::Open { last_message_id } = ev {
            return Some(last_message_id);
        }
    }
    None
}

#[tokio::test]
async fn ws_handshake_sends_login_and_receives_open() {
    // Mock: read the login line, assert its wire shape, reply with open, then
    // hold the connection open until the client goes away.
    let (addr, _srv) = start_mock(|_n, mut ws| async move {
        let login = match ws.next().await {
            Some(Ok(Message::Text(t))) => t,
            _ => panic!("expected a login text frame"),
        };
        let s = login.as_str();
        assert!(
            s.contains(r#""type":"login""#),
            "login frame must be tagged type=login, got: {s}"
        );
        assert!(
            s.contains(r#""device_id":"dev""#),
            "login frame must carry device_id, got: {s}"
        );
        assert!(
            s.contains(r#""secret":"sec""#),
            "login frame must carry secret, got: {s}"
        );
        assert!(s.ends_with('\n'), "login line must be newline-terminated");

        ws.send(Message::Text(
            r#"{"type":"open","last_message_id":42}"#.into(),
        ))
        .await
        .unwrap();
        // Hold the socket open so the client doesn't see an immediate close.
        while ws.next().await.is_some() {}
    })
    .await;

    let client = Arc::new(
        Client::new(WsConfig::new(
            format!("ws://{}", addr),
            "dev".into(),
            "sec".into(),
        ))
        .unwrap(),
    );
    let (mut rx, sd, task) = spawn_client(client);

    let last_id = tokio::time::timeout(TEST_DEADLINE, drain_until_open(&mut rx))
        .await
        .expect("timed out waiting for open frame")
        .expect("stream ended before open frame");
    assert_eq!(last_id, 42, "open frame carries the server high-water mark");

    let _ = sd.send(());
    let _ = tokio::time::timeout(TEST_DEADLINE, task).await;
}

#[tokio::test]
async fn ws_since_replay_decodes_and_persists_cursor() {
    // Pre-seed the cursor with id=100; the client must request since=100 (the
    // URL contract is asserted by build_ws_url_appends_since_query) and the
    // server replays messages with id > 100 in ascending order.
    let cursor_path = unique_tmp_path();
    std::fs::write(&cursor_path, "100").unwrap();

    let (addr, _srv) = start_mock(|_n, mut ws| async move {
        let _ = ws.next().await; // consume login
        ws.send(Message::Text(
            r#"{"type":"open","last_message_id":102}"#.into(),
        ))
        .await
        .unwrap();
        ws.send(Message::Text(
            r#"{"type":"message","id":101,"send_id":1,"priority":0,"message":"a","timestamp":1}"#
                .into(),
        ))
        .await
        .unwrap();
        ws.send(Message::Text(
            r#"{"type":"message","id":102,"send_id":1,"priority":1,"title":"t","message":"b","timestamp":2,"sound":"pushover"}"#
                .into(),
        ))
        .await
        .unwrap();
        while ws.next().await.is_some() {}
    })
    .await;

    let mut cfg = WsConfig::new(format!("ws://{}", addr), "dev".into(), "sec".into());
    cfg.cursor_path = Some(cursor_path.clone());
    let client = Arc::new(Client::new(cfg).unwrap());
    let (mut rx, sd, task) = spawn_client(client.clone());

    let mut ids = Vec::new();
    let mut last_msg = None;
    let observed = tokio::time::timeout(TEST_DEADLINE, async {
        while let Some(ev) = rx.recv().await {
            if let Event::Message(m) = ev {
                ids.push(m.id);
                last_msg = Some(m);
                if ids.len() == 2 {
                    break;
                }
            }
        }
    })
    .await;
    assert!(observed.is_ok(), "timed out waiting for replayed messages");

    // Replay arrives in ascending id order, only ids strictly greater than the
    // seeded cursor (101, 102 — not anything <= 100).
    assert_eq!(ids, vec![101, 102], "replay order and id range");

    // Decoded payload (manual QA: mock sends message -> client decodes).
    let m = last_msg.expect("second message decoded");
    assert_eq!(m.id, 102);
    assert_eq!(m.message, "b");
    assert_eq!(m.title, "t");
    assert_eq!(m.priority, 1);
    assert_eq!(m.sound, "pushover");

    // Cursor advanced to the highest observed id and persisted to disk.
    assert_eq!(client.cursor.load(), 102);
    assert_eq!(std::fs::read_to_string(&cursor_path).unwrap().trim(), "102");

    let _ = sd.send(());
    let _ = tokio::time::timeout(TEST_DEADLINE, task).await;
    let _ = std::fs::remove_file(&cursor_path);
}

#[tokio::test]
async fn ws_reconnect_after_close_4001_uses_full_jitter_backoff() {
    // conn 0: auth failure -> application close 4001.
    // conn 1+: accept, deliver one message, hold open.
    let (addr, _srv) = start_mock(|n, mut ws| async move {
        let _ = ws.next().await; // consume login
        if n == 0 {
            ws.send(Message::Close(Some(CloseFrame {
                code: CloseCode::from(4001u16),
                reason: "invalid device_id or secret".into(),
            })))
            .await
            .unwrap();
            return;
        }
        ws.send(Message::Text(
            r#"{"type":"open","last_message_id":0}"#.into(),
        ))
        .await
        .unwrap();
        ws.send(Message::Text(
            r#"{"type":"message","id":7,"send_id":1,"priority":0,"message":"hello","timestamp":1}"#
                .into(),
        ))
        .await
        .unwrap();
        while ws.next().await.is_some() {}
    })
    .await;

    let mut cfg = WsConfig::new(format!("ws://{}", addr), "dev".into(), "sec".into());
    cfg.backoff_base = Duration::from_millis(1);
    cfg.backoff_cap = Duration::from_millis(5);
    let client = Arc::new(Client::new(cfg).unwrap());
    let (mut rx, sd, task) = spawn_client(client);

    // Collect events until the post-reconnect message lands.
    let mut events = Vec::new();
    let observed = tokio::time::timeout(TEST_DEADLINE, async {
        while let Some(ev) = rx.recv().await {
            let done = matches!(&ev, Event::Message(m) if m.id == 7);
            events.push(ev);
            if done {
                break;
            }
        }
    })
    .await;
    assert!(observed.is_ok(), "timed out; events so far: {:?}", events);

    // Failure path: close 4001 -> backoff(attempt 0) -> reconnect -> delivery.
    let close4001 = events.iter().position(|e| {
        matches!(
            e,
            Event::Disconnected {
                close_code: Some(4001),
                ..
            }
        )
    });
    let backoff0 = events
        .iter()
        .position(|e| matches!(e, Event::BackoffScheduled { attempt: 0, .. }));
    let msg = events
        .iter()
        .position(|e| matches!(e, Event::Message(ServerMessage { id: 7, .. })));
    assert!(
        close4001.is_some(),
        "expected Disconnected(close=4001): {events:?}"
    );
    assert!(
        backoff0.is_some(),
        "expected BackoffScheduled(attempt 0): {events:?}"
    );
    assert!(msg.is_some(), "expected delivered message id 7: {events:?}");
    assert!(
        close4001.unwrap() < msg.unwrap(),
        "close 4001 must precede the reconnect delivery"
    );

    // The scheduled backoff must respect Full-Jitter bounds: [0, ceiling(0)].
    let delay = events
        .iter()
        .find_map(|e| match e {
            Event::BackoffScheduled { attempt: 0, delay } => Some(*delay),
            _ => None,
        })
        .expect("attempt-0 backoff present");
    let ceil = backoff_ceiling(0, Duration::from_millis(1), Duration::from_millis(5));
    assert!(
        delay <= ceil,
        "backoff {delay:?} must not exceed Full-Jitter ceiling {ceil:?}"
    );

    let _ = sd.send(());
    let _ = tokio::time::timeout(TEST_DEADLINE, task).await;
}

#[tokio::test]
async fn ws_read_timeout_reconnects_on_silent_server() {
    // Server accepts, consumes login, then sends nothing and never closes. The
    // client's injected (small) read timeout must fire and trigger reconnect.
    let (addr, _srv) = start_mock(|_n, mut ws| async move {
        let _ = ws.next().await; // consume login, then stay silent.
        while ws.next().await.is_some() {}
    })
    .await;

    let mut cfg = WsConfig::new(format!("ws://{}", addr), "dev".into(), "sec".into());
    cfg.read_timeout = Duration::from_millis(75);
    cfg.backoff_base = Duration::from_millis(1);
    cfg.backoff_cap = Duration::from_millis(5);
    let client = Arc::new(Client::new(cfg).unwrap());
    let (mut rx, sd, task) = spawn_client(client);

    // Wait for at least one ReadTimeout + a follow-up BackoffScheduled.
    let observed = tokio::time::timeout(TEST_DEADLINE, async {
        let mut saw_timeout = false;
        while let Some(ev) = rx.recv().await {
            match ev {
                Event::ReadTimeout { .. } => saw_timeout = true,
                Event::BackoffScheduled { .. } if saw_timeout => return true,
                _ => {}
            }
        }
        false
    })
    .await;
    assert!(
        observed.is_ok() && observed.unwrap(),
        "expected ReadTimeout followed by BackoffScheduled from the silent server"
    );

    let _ = sd.send(());
    let _ = tokio::time::timeout(TEST_DEADLINE, task).await;
}

#[tokio::test]
async fn ws_keepalive_frame_is_decoded() {
    // Server sends open + a keepalive line; client must surface Keepalive.
    let (addr, _srv) = start_mock(|_n, mut ws| async move {
        let _ = ws.next().await; // login
        ws.send(Message::Text(
            r#"{"type":"open","last_message_id":0}"#.into(),
        ))
        .await
        .unwrap();
        ws.send(Message::Text(r#"{"type":"keepalive"}"#.into()))
            .await
            .unwrap();
        while ws.next().await.is_some() {}
    })
    .await;

    let client = Arc::new(
        Client::new(WsConfig::new(
            format!("ws://{}", addr),
            "dev".into(),
            "sec".into(),
        ))
        .unwrap(),
    );
    let (mut rx, sd, task) = spawn_client(client);

    let observed = tokio::time::timeout(TEST_DEADLINE, async {
        while let Some(ev) = rx.recv().await {
            if matches!(ev, Event::Keepalive) {
                return true;
            }
        }
        false
    })
    .await;
    assert!(
        observed.is_ok() && observed.unwrap(),
        "expected a Keepalive event"
    );

    let _ = sd.send(());
    let _ = tokio::time::timeout(TEST_DEADLINE, task).await;
}
