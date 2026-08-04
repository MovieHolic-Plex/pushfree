package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pushfree/pushfree/internal/callbacks"
	"github.com/pushfree/pushfree/internal/store"
)

// CallbackWorkerRepo is the Postgres implementation of callbacks.Store.
// It mirrors the SQLite CallbackWorkerRepo, substituting $N placeholders
// and Postgres-native timestamp handling.
type CallbackWorkerRepo struct{ db DB }

// NewCallbackWorkerRepo builds a CallbackWorkerRepo over an already-open DB.
func NewCallbackWorkerRepo(db DB) *CallbackWorkerRepo { return &CallbackWorkerRepo{db: db} }

func (c *CallbackWorkerRepo) GetTarget(ctx context.Context, receiptID string) (callbacks.Target, error) {
	row := c.db.QueryRowContext(ctx, `
SELECT r.id, r.send_id, r.state, r.tag, r.retry_count, r.expires_at, r.acknowledged_at,
	r.acknowledged_by, r.acknowledged_by_device, r.last_delivered_at, r.called_back_at,
	r.expired_at, r.canceled_at, s.callback_url
FROM receipts r JOIN sends s ON r.send_id = s.id
WHERE r.id = $1`, receiptID)
	var (
		rc  store.Receipt
		tag sql.NullString
		expiresAt,
		ackAt,
		lastDeliv,
		calledBack,
		expiredAt,
		canceledAt sql.NullTime
		ackBy,
		ackByDev,
		cbURL sql.NullString
	)
	if err := row.Scan(&rc.ID, &rc.SendID, &rc.State, &tag, &rc.RetryCount, &expiresAt, &ackAt,
		&ackBy, &ackByDev, &lastDeliv, &calledBack, &expiredAt, &canceledAt, &cbURL); err != nil {
		return callbacks.Target{}, mapErr(err)
	}
	rc.Tag = nullStr(tag)
	rc.ExpiresAt = nullTime(expiresAt)
	rc.AcknowledgedAt = nullTime(ackAt)
	rc.AcknowledgedBy = nullStr(ackBy)
	rc.AcknowledgedByDevice = nullStr(ackByDev)
	rc.LastDeliveredAt = nullTime(lastDeliv)
	rc.CalledBackAt = nullTime(calledBack)
	rc.ExpiredAt = nullTime(expiredAt)
	rc.CanceledAt = nullTime(canceledAt)
	return callbacks.Target{Receipt: rc, CallbackURL: nullStr(cbURL)}, nil
}

func (c *CallbackWorkerRepo) CreateCallback(ctx context.Context, receiptID, u string, now time.Time) (int64, error) {
	var id int64
	err := c.db.QueryRowContext(ctx,
		`INSERT INTO callbacks(receipt_id, url, state, next_attempt_at, attempts)
		 VALUES ($1, $2, 'pending', $3, 0) RETURNING id`,
		receiptID, u, now).Scan(&id)
	if err != nil {
		return 0, mapErr(err)
	}
	return id, nil
}

func (c *CallbackWorkerRepo) ListDue(ctx context.Context, now time.Time, limit int) ([]store.Callback, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT id, receipt_id, url, state, next_attempt_at, attempts FROM callbacks
WHERE state <> 'done' AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
ORDER BY (next_attempt_at IS NULL) DESC, next_attempt_at, id
LIMIT $2`, now, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Callback
	for rows.Next() {
		var (
			cb   store.Callback
			next sql.NullTime
		)
		if err := rows.Scan(&cb.ID, &cb.ReceiptID, &cb.URL, &cb.State, &next, &cb.Attempts); err != nil {
			return nil, mapErr(err)
		}
		cb.NextAttemptAt = nullTime(next)
		out = append(out, cb)
	}
	return out, mapErr(rows.Err())
}

func (c *CallbackWorkerRepo) MarkFailed(ctx context.Context, id int64, next time.Time, attempts int) error {
	if _, err := c.db.ExecContext(ctx,
		`UPDATE callbacks SET state='failed', next_attempt_at=$1, attempts=$2 WHERE id=$3`,
		next, attempts, id); err != nil {
		return mapErr(err)
	}
	return nil
}

func (c *CallbackWorkerRepo) MarkDone(ctx context.Context, id int64, receiptID string, at time.Time) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("callback mark done: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE callbacks SET state='done', next_attempt_at=NULL WHERE id=$1`, id); err != nil {
		return mapErr(err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE receipts SET called_back_at=$1 WHERE id=$2`, at, receiptID); err != nil {
		return mapErr(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("callback mark done: commit: %w", err)
	}
	return nil
}

func (c *CallbackWorkerRepo) RecordDLQ(ctx context.Context, callbackID int64, lastErr string, at time.Time, attempts int) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO dlq(callback_id, last_error, at, attempts) VALUES ($1, $2, $3, $4)`,
		callbackID, lastErr, at, attempts)
	return mapErr(err)
}
