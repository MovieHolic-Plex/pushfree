package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// TimerRepo is the SQLite implementation of store.TimerRepo.
type TimerRepo struct{ db DB }

func (t *TimerRepo) Create(ctx context.Context, in *store.Timer) (int64, error) {
	res, err := t.db.ExecContext(ctx,
		`INSERT INTO timers(kind, receipt_id, fire_at, payload, claimed_at) VALUES (?, ?, ?, ?, ?)`,
		in.Kind, optStr(in.ReceiptID), rfc3339(in.FireAt), in.Payload, nullTimePtr(in.ClaimedAt))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("timer last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

// ClaimDue atomically claims up to limit due, unclaimed timers (fire_at <= now
// AND claimed_at IS NULL), marking claimed_at = now and returning the claimed
// rows. The claim is a single UPDATE ... RETURNING so under the SQLite
// single-writer lock each timer is handed to exactly one caller, even when
// multiple workers poll concurrently (busy_timeout serializes the writers).
//
// The partial index idx_timers_due (fire_at WHERE claimed_at IS NULL) keeps
// the due-set lookup cheap as the table grows.
func (t *TimerRepo) ClaimDue(ctx context.Context, now time.Time, limit int) ([]store.Timer, error) {
	if limit <= 0 {
		limit = 100
	}
	nowStr := rfc3339(now)
	rows, err := t.db.QueryContext(ctx, `
UPDATE timers SET claimed_at = ?
WHERE id IN (
	SELECT id FROM timers
	WHERE fire_at <= ? AND claimed_at IS NULL
	ORDER BY id
	LIMIT ?
)
RETURNING id, kind, receipt_id, fire_at, payload, claimed_at`,
		nowStr, nowStr, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.Timer, 0, limit)
	for rows.Next() {
		tm, err := scanTimer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tm)
	}
	return out, mapErr(rows.Err())
}

// scanTimer scans a timer row from s.
func scanTimer(s scanner) (store.Timer, error) {
	var (
		tm        store.Timer
		receiptID sql.NullString
		fireAt    sql.NullString
		claimedAt sql.NullString
	)
	if err := s.Scan(&tm.ID, &tm.Kind, &receiptID, &fireAt, &tm.Payload, &claimedAt); err != nil {
		return store.Timer{}, mapErr(err)
	}
	tm.ReceiptID = nullStr(receiptID)
	if v, ok := parseTime(fireAt.String, fireAt.Valid); ok {
		tm.FireAt = v
	}
	tm.ClaimedAt = nullTime(claimedAt)
	return tm, nil
}
