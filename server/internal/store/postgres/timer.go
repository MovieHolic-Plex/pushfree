package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// TimerRepo is the Postgres implementation of store.TimerRepo.
type TimerRepo struct{ db DB }

// Create inserts a timer row and writes the assigned id back to in.ID.
func (t *TimerRepo) Create(ctx context.Context, in *store.Timer) (int64, error) {
	err := t.db.QueryRowContext(ctx,
		`INSERT INTO timers(kind, receipt_id, fire_at, payload, claimed_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		in.Kind, optStr(in.ReceiptID), in.FireAt, in.Payload, timeArg(in.ClaimedAt)).
		Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

// ClaimDue atomically claims up to limit due, unclaimed timers (fire_at <= now
// AND claimed_at IS NULL), marking claimed_at = now and returning the claimed
// rows. The claim is a single UPDATE ... RETURNING; the inner SELECT uses FOR
// UPDATE SKIP LOCKED so multiple workers polling concurrently each claim a
// disjoint set (the Postgres-native equivalent of the SQLite single-writer
// guarantee).
func (t *TimerRepo) ClaimDue(ctx context.Context, now time.Time, limit int) ([]store.Timer, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := t.db.QueryContext(ctx, `
UPDATE timers SET claimed_at = $1
WHERE id IN (
	SELECT id FROM timers
	WHERE fire_at <= $2 AND claimed_at IS NULL
	ORDER BY id
	LIMIT $3
	FOR UPDATE SKIP LOCKED
)
RETURNING id, kind, receipt_id, fire_at, payload, claimed_at`,
		now, now, limit)
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
		fireAt    sql.NullTime
		claimedAt sql.NullTime
	)
	if err := s.Scan(&tm.ID, &tm.Kind, &receiptID, &fireAt, &tm.Payload, &claimedAt); err != nil {
		return store.Timer{}, mapErr(err)
	}
	tm.ReceiptID = nullStr(receiptID)
	if fireAt.Valid {
		tm.FireAt = fireAt.Time
	}
	tm.ClaimedAt = nullTime(claimedAt)
	return tm, nil
}

// Delete removes one timer row by id. It is not an error if the id is absent
// (idempotent delete). Used by the timer engine after a timer fires.
func (t *TimerRepo) Delete(ctx context.Context, id int64) error {
	if _, err := t.db.ExecContext(ctx, `DELETE FROM timers WHERE id = $1`, id); err != nil {
		return mapErr(err)
	}
	return nil
}

// ResetOrphanedClaims clears claimed_at on every currently-claimed timer,
// returning the count of rows reset. Called once at startup so timers left
// claimed by a crashed worker re-enter the due-set and fire exactly once.
func (t *TimerRepo) ResetOrphanedClaims(ctx context.Context) (int, error) {
	res, err := t.db.ExecContext(ctx, `UPDATE timers SET claimed_at = NULL WHERE claimed_at IS NOT NULL`)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}
