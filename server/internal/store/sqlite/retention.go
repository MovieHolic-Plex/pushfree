package sqlite

import (
	"context"
	"fmt"
	"time"
)

// This file holds the append-only retention/TTL/checkpoint store methods
// (todo 18). They are defined on *Store so no existing file in this package
// changes; the sweeper (internal/retention) depends on the narrow interface
// these methods collectively satisfy.
//
// Time comparisons use strftime('%s', col) (epoch seconds) rather than raw
// RFC3339 text comparison, so the result is independent of any sub-second
// formatting differences in the stored TEXT. See util.go for the rfc3339
// convention. All three sweep statements run under SQLite's busy_timeout
// (5000ms, set in buildDSN), so a concurrent writer is serialized rather
// than surfacing as SQLITE_BUSY.

// DeleteMessagesBefore deletes per-recipient message rows whose created_at is
// strictly older than cutoff, returning the number of rows deleted. Only
// message rows are removed; the parent sends and any receipts are left for
// the receipts subsystem to GC (todo 23). Backs the messages-retention sweep
// (default 30 days).
//
// DEV NOTE (API-COMPAT): Pushover retains delivered messages for 21 days;
// pushfree defaults to 30 days (messages-retention). Record this deviation
// in docs/API-COMPAT.md when that doc is written (todo 49).
func (s *Store) DeleteMessagesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM messages
WHERE CAST(strftime('%s', created_at) AS INTEGER) < CAST(strftime('%s', ?) AS INTEGER)`
	res, err := s.db.ExecContext(ctx, q, rfc3339(cutoff))
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention messages rows affected: %w", err)
	}
	return n, nil
}

// ClearUndownloadedAttachmentBLOBs zeroes the data BLOB of attachment rows
// whose parent send is older than cutoff AND that have never been downloaded
// (downloaded_at IS NULL). The ROW is intentionally kept (callers can still
// see the attachment exists and its content-type); only the bytes are dropped
// to bound on-disk growth. Returns the number of BLOBs cleared. Backs the
// 3-day undownloaded-attachment rule.
//
// The `length(data) > 0` guard makes a sweep idempotent: re-running it on
// already-cleared rows is a no-op write (count 0), so the hourly sweeper may
// fire any number of times without write amplification.
func (s *Store) ClearUndownloadedAttachmentBLOBs(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `UPDATE attachments
SET data = x''
WHERE downloaded_at IS NULL
  AND length(data) > 0
  AND send_id IN (
    SELECT id FROM sends
    WHERE CAST(strftime('%s', created_at) AS INTEGER) < CAST(strftime('%s', ?) AS INTEGER)
  )`
	res, err := s.db.ExecContext(ctx, q, rfc3339(cutoff))
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention attachments rows affected: %w", err)
	}
	return n, nil
}

// DeleteUndeliveredExpiredByTTL deletes message rows that are still
// undelivered (delivered_at IS NULL) and whose parent send's TTL has elapsed
// as of now. A send expires at created_at + ttl seconds; only sends with
// ttl > 0 ever expire. Delivered messages are always kept, even after their
// TTL window closes. Returns the number of message rows discarded. Backs the
// sends.ttl discard rule.
func (s *Store) DeleteUndeliveredExpiredByTTL(ctx context.Context, now time.Time) (int64, error) {
	const q = `DELETE FROM messages
WHERE delivered_at IS NULL
  AND send_id IN (
    SELECT id FROM sends
    WHERE ttl > 0
      AND CAST(strftime('%s', created_at) AS INTEGER) + ttl
          < CAST(strftime('%s', ?) AS INTEGER)
  )`
	res, err := s.db.ExecContext(ctx, q, rfc3339(now))
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention ttl rows affected: %w", err)
	}
	return n, nil
}

// WALCheckpointResult is the three-value return of PRAGMA wal_checkpoint:
// busy (0 = ok, 1 = a reader/writer was active), log (WAL frames), and
// checkpointed (frames moved back into the main db). Logged at shutdown as
// WAL-checkpoint evidence (todo 18 SIGTERM acceptance).
type WALCheckpointResult struct {
	Busy         int
	Log          int
	Checkpointed int
}

// WALCheckpoint forces a TRUNCATE-mode WAL checkpoint, which moves all
// committed WAL frames back into the main database file and truncates the
// -wal sidecar. It is the last step of graceful shutdown so a cleanly stopped
// server leaves a single consistent .db file (no pending WAL to replay). It
// is best-effort: a returned Busy=1 is reported but not an error, since the
// database is still consistent.
func (s *Store) WALCheckpoint(ctx context.Context) (WALCheckpointResult, error) {
	// PRAGMA cannot be parameterized; the mode is a fixed literal.
	var r WALCheckpointResult
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&r.Busy, &r.Log, &r.Checkpointed); err != nil {
		return WALCheckpointResult{}, fmt.Errorf("wal checkpoint: %w", err)
	}
	return r, nil
}
