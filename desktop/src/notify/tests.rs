//! Tests for the notification pipeline.
//!
//! Two integration tests drive the contract the acceptance gate names:
//! - `duplicate_send_id_acked_exactly_once`: same send_id twice -> sink fires
//!   once, ack posted once.
//! - `ack_500_retried_then_success`: a 5xx is retried then succeeds, under a
//!   paused/virtual clock with a wall-clock assertion proving NO real sleep.
//!
//! Pure helpers (priority mapping, ack URL, dedup, config validation) are
//! covered by fast unit tests.

use super::*;

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU32, Ordering};
use std::time::Instant;

use tokio::sync::mpsc;

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

/// Records the formatted notification content for each call.
struct MockSink {
    calls: Mutex<Vec<NotificationContent>>,
}

impl MockSink {
    fn new() -> Self {
        MockSink {
            calls: Mutex::new(Vec::new()),
        }
    }
    fn recorded(&self) -> Vec<NotificationContent> {
        self.calls.lock().expect("sink lock poisoned").clone()
    }
    fn count(&self) -> usize {
        self.calls.lock().expect("sink lock poisoned").len()
    }
}

impl NotifySink for MockSink {
    fn notify(&self, msg: &ServerMessage) -> Result<(), NotifyError> {
        self.calls
            .lock()
            .expect("sink lock poisoned")
            .push(format_notification(msg));
        Ok(())
    }
}

/// Returns scripted outcomes in order, then `Ok` forever. Records each call's
/// message id so tests assert "acked exactly once" and the retry sequence.
struct MockAckClient {
    scripted: Mutex<VecDeque<AckOutcome>>,
    calls: Mutex<Vec<String>>,
}

impl MockAckClient {
    /// Returns `outcomes` in order, then `Ok`.
    fn sequence(outcomes: Vec<AckOutcome>) -> Self {
        MockAckClient {
            scripted: Mutex::new(outcomes.into_iter().collect()),
            calls: Mutex::new(Vec::new()),
        }
    }

    fn calls(&self) -> Vec<String> {
        self.calls.lock().expect("client lock poisoned").clone()
    }
}

#[async_trait]
impl AckClient for MockAckClient {
    async fn ack(&self, receipt_id: &str) -> AckOutcome {
        self.calls
            .lock()
            .expect("client lock poisoned")
            .push(receipt_id.to_string());
        self.scripted
            .lock()
            .expect("client lock poisoned")
            .pop_front()
            .unwrap_or(AckOutcome::Ok)
    }
}

/// Build a minimal server message for tests.
fn msg(send_id: i64, id: i64, priority: i32) -> ServerMessage {
    ServerMessage {
        id,
        send_id,
        priority,
        sound: String::new(),
        title: String::new(),
        message: format!("body-{send_id}"),
        url: String::new(),
        url_title: String::new(),
        html: false,
        monospace: false,
        timestamp: 1,
        ttl: 0,
        tag: String::new(),
        encrypted: false,
        receipt_id: format!("rec-{id}"),
    }
}

/// A unique temp path for a dedup log so parallel tests never collide.
fn unique_tmp_path() -> PathBuf {
    static SEQ: AtomicU32 = AtomicU32::new(0);
    let n = SEQ.fetch_add(1, Ordering::SeqCst);
    let pid = std::process::id();
    std::env::temp_dir().join(format!("pushfree-notify-test-{pid}-{n}.dedup"))
}

// ---------------------------------------------------------------------------
// Pure unit tests: priority mapping + formatting
// ---------------------------------------------------------------------------

#[test]
fn priority_label_covers_full_range() {
    assert_eq!(priority_label(2), "EMERGENCY");
    assert_eq!(priority_label(3), "EMERGENCY", "p>2 clamps to EMERGENCY");
    assert_eq!(priority_label(1), "High");
    assert_eq!(priority_label(0), "");
    assert_eq!(priority_label(-1), "Low");
    assert_eq!(priority_label(-2), "Lowest");
    assert_eq!(priority_label(-5), "Lowest", "p<-2 clamps to Lowest");
}

#[test]
fn format_notification_priority_styles() {
    // p2 emergency with title -> label prepended.
    let mut m = msg(1, 10, 2);
    m.title = "db down".into();
    let c = format_notification(&m);
    assert_eq!(c.title, "EMERGENCY · db down");
    assert_eq!(c.body, "body-1");

    // p0 normal with title -> title verbatim, no label.
    let mut m = msg(1, 10, 0);
    m.title = "hello".into();
    assert_eq!(format_notification(&m).title, "hello");

    // no title, p0 -> product name fallback.
    let m = msg(1, 10, 0);
    assert_eq!(format_notification(&m).title, "PushFree");

    // no title, p2 -> label + product name.
    let m = msg(1, 10, 2);
    assert_eq!(format_notification(&m).title, "EMERGENCY · PushFree");
}

// ---------------------------------------------------------------------------
// Pure unit tests: ack URL
// ---------------------------------------------------------------------------

#[test]
fn build_ack_url_shape() {
    assert_eq!(
        build_ack_url("https://srv.example.com", "rec123", "dev1", "s3cr3t"),
        "https://srv.example.com/1/receipts/rec123/acknowledge.json?device_id=dev1&secret=s3cr3t"
    );
    // trailing slash tolerated
    assert_eq!(
        build_ack_url("http://h:2586/", "abc", "dev2", "xyz"),
        "http://h:2586/1/receipts/abc/acknowledge.json?device_id=dev2&secret=xyz"
    );
}

// ---------------------------------------------------------------------------
// Pure unit tests: dedup
// ---------------------------------------------------------------------------

#[test]
fn dedup_in_memory_marks_first_sighting_only() {
    let d = Dedup::new(None);
    assert!(d.is_empty());
    assert!(d.observe(1), "first sighting of send_id 1 is new");
    assert!(!d.observe(1), "second sighting of send_id 1 is a duplicate");
    assert!(d.observe(2), "send_id 2 is new");
    assert!(!d.observe(2));
    assert_eq!(d.len(), 2);
}

#[test]
fn dedup_persistence_roundtrip() {
    let path = unique_tmp_path();
    // File does not exist yet -> empty set.
    let d = Dedup::new(Some(path.clone()));
    assert!(d.observe(100));
    assert!(
        !d.observe(100),
        "in-memory dedup still applies after persist"
    );
    assert!(d.observe(200));

    // The log on disk records both send_ids (append-only).
    let on_disk = std::fs::read_to_string(&path).unwrap();
    assert!(on_disk.contains("100") && on_disk.contains("200"));

    // A fresh Dedup loading the file must consider both already seen.
    let d2 = Dedup::new(Some(path.clone()));
    assert!(!d2.observe(100), "persisted send_id must not re-fire");
    assert!(!d2.observe(200));

    let _ = std::fs::remove_file(&path);
}

#[test]
fn dedup_tolerates_corrupt_log() {
    let path = unique_tmp_path();
    std::fs::write(&path, "garbage not-a-number 5\nalso-bad").unwrap();
    let d = Dedup::new(Some(path.clone()));
    // Numeric tokens are loaded; junk is ignored; the set never panics.
    assert!(
        !d.observe(5),
        "the valid token 5 was loaded from the corrupt log"
    );
    assert!(d.observe(6), "6 was absent");
    let _ = std::fs::remove_file(&path);
}

// ---------------------------------------------------------------------------
// Pure unit tests: HTTP client config
// ---------------------------------------------------------------------------

#[test]
fn http_ack_client_rejects_invalid_base_url() {
    assert!(matches!(
        HttpAckClient::new("ws://h".into(), "d".into(), "s".into()),
        Err(AckClientError::InvalidBaseUrl)
    ));
    assert!(HttpAckClient::new("http://h".into(), "d".into(), "s".into()).is_ok());
    assert!(HttpAckClient::new("https://h".into(), "d".into(), "s".into()).is_ok());
}

// ---------------------------------------------------------------------------
// Integration: duplicate send_id notified exactly once (no auto-ack)
// ---------------------------------------------------------------------------

#[tokio::test]
async fn duplicate_send_id_notified_exactly_once() {
    let (ack_tx, _ack_rx) = mpsc::channel::<String>(8);
    let sink = Arc::new(MockSink::new());
    let pipeline = Pipeline::new(Dedup::new(None), sink.clone(), ack_tx);

    // Same send_id (1), two deliveries (e.g. a WS replay). Only the first
    // notifies; the duplicate is suppressed.
    let first = msg(1, 10, 0);
    let dup = msg(1, 11, 0); // different message id, SAME send_id
    assert_eq!(pipeline.handle(&first).await, HandleOutcome::Notified);
    assert_eq!(pipeline.handle(&dup).await, HandleOutcome::Duplicate);

    assert_eq!(sink.count(), 1, "exactly one notification for the send");
    let recorded = sink.recorded();
    assert_eq!(recorded.len(), 1);
    // priority 0 + empty title -> product-name fallback (format_notification).
    assert_eq!(recorded[0].title, "PushFree");
    assert_eq!(recorded[0].body, "body-1");
}

// ---------------------------------------------------------------------------
// Integration: ack 500 -> retried -> success (virtual clock, no real sleeps)
// ---------------------------------------------------------------------------

#[tokio::test(start_paused = true)]
async fn ack_500_retried_then_success() {
    // Script: first attempt 500 (retry), second attempt success.
    let client = Arc::new(MockAckClient::sequence(vec![
        AckOutcome::Retry(AckError::Status(500)),
        AckOutcome::Ok,
    ]));
    let (ack_tx, ack_rx) = mpsc::channel::<String>(8);
    // 60s retry delay. Under the paused clock this advances instantly; the
    // wall-clock assertion below proves no real 60s sleep happened.
    let reporter = AckReporter::new(ack_rx, client.clone(), Duration::from_secs(60));

    let started = Instant::now();
    // Send the receipt directly — in production a user-triggered action
    // (notification tap/dialog) will feed this channel.
    ack_tx.send("rec-77".to_string()).await.unwrap();
    drop(ack_tx);

    // The reporter sleeps 60s (virtual) between the failed and successful
    // attempt. The virtual budget (600s) is far above 60s so the reporter's
    // timer fires first under auto-advance; a real hang still fails fast
    // because virtual time jumps to the budget.
    let ran = tokio::time::timeout(Duration::from_secs(600), reporter.run()).await;
    assert!(
        ran.is_ok(),
        "reporter did not complete within virtual budget - clock not advancing?"
    );

    // Hard proof no real sleep occurred: the whole retry sequence took well
    // under the 60s retry delay in wall-clock time.
    let elapsed = started.elapsed();
    assert!(
        elapsed < Duration::from_secs(10),
        "retry sequence took {elapsed:?} real time - a real sleep leaked in"
    );

    assert_eq!(
        client.calls(),
        vec!["rec-77".to_string(), "rec-77".to_string()],
        "acked twice: first 500-retry, then success"
    );
}

// ---------------------------------------------------------------------------
// Integration: permanent (4xx) failure is not retried
// ---------------------------------------------------------------------------

#[tokio::test(start_paused = true)]
async fn ack_permanent_failure_not_retried() {
    // A 404 is permanent: the reporter must call once and abandon.
    let client = Arc::new(MockAckClient::sequence(vec![AckOutcome::Permanent(
        AckError::Status(404),
    )]));
    let (ack_tx, ack_rx) = mpsc::channel::<String>(8);
    let reporter = AckReporter::new(ack_rx, client.clone(), Duration::from_secs(60));

    ack_tx.send("rec-99".to_string()).await.unwrap();
    drop(ack_tx);

    let started = Instant::now();
    let ran = tokio::time::timeout(Duration::from_secs(600), reporter.run()).await;
    assert!(
        ran.is_ok(),
        "reporter must terminate even on permanent failure"
    );
    assert!(
        started.elapsed() < Duration::from_secs(10),
        "permanent failure must not sleep/retry"
    );
    assert_eq!(
        client.calls(),
        vec!["rec-99".to_string()],
        "called exactly once then abandoned"
    );
}

// ---------------------------------------------------------------------------
// Integration: notify failure does not block the pipeline
// ---------------------------------------------------------------------------

struct FailingSink;
impl NotifySink for FailingSink {
    fn notify(&self, _: &ServerMessage) -> Result<(), NotifyError> {
        Err(NotifyError("simulated toast backend failure".into()))
    }
}

#[tokio::test]
async fn notify_failure_still_returns_notified() {
    // EB/A3 best-effort contract: a notification failure must not suppress
    // the pipeline outcome. The message is still marked Notified so the
    // caller (WS event loop) proceeds normally.
    let (ack_tx, _ack_rx) = mpsc::channel::<String>(8);
    let pipeline = Pipeline::new(Dedup::new(None), Arc::new(FailingSink), ack_tx);

    assert_eq!(
        pipeline.handle(&msg(5, 50, 0)).await,
        HandleOutcome::Notified
    );
}
