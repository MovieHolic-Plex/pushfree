package postgres

import (
	"context"
	"fmt"
	"time"
)

// This file implements the retention.Store interface for Postgres so the
// retention sweeper works with both backends. The SQL mirrors the SQLite
// implementation (retention.go in the sqlite package), substituting
// standard SQL timestamp comparison for SQLite's strftime epoch casts.

// DeleteMessagesBefore deletes message rows older than cutoff.
func (s *Store) DeleteMessagesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention messages rows affected: %w", err)
	}
	return n, nil
}

// ClearUndownloadedAttachmentBLOBs zeroes the data of attachment rows whose
// parent send is older than cutoff and that have never been downloaded.
func (s *Store) ClearUndownloadedAttachmentBLOBs(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE attachments SET data = $1
WHERE downloaded_at IS NULL
  AND length(data) > 0
  AND send_id IN (
    SELECT id FROM sends WHERE created_at < $2
  )`, make([]byte, 0), cutoff)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention attachments rows affected: %w", err)
	}
	return n, nil
}

// DeleteUndeliveredExpiredByTTL deletes undelivered message rows whose parent
// send's TTL has elapsed.
func (s *Store) DeleteUndeliveredExpiredByTTL(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM messages
WHERE delivered_at IS NULL
  AND send_id IN (
    SELECT id FROM sends
    WHERE ttl > 0 AND created_at + (ttl || ' seconds')::interval < $1
  )`, now)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention ttl rows affected: %w", err)
	}
	return n, nil
}
