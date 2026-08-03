package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// ReceiptRepo is the Postgres implementation of store.ReceiptRepo.
type ReceiptRepo struct{ db DB }

// Create inserts a receipt row. Enforces the receipt<->send 1:1 via the
// UNIQUE(receipts.send_id) constraint.
func (r *ReceiptRepo) Create(ctx context.Context, in *store.Receipt) error {
	return insertReceipt(ctx, r.db, in)
}

// insertReceipt inserts a receipt row. Shared with the fan-out / ingest paths.
func insertReceipt(ctx context.Context, q queryExec, in *store.Receipt) error {
	_, err := q.ExecContext(ctx, `
INSERT INTO receipts(id, send_id, state, tag, retry_count, expires_at, acknowledged_at,
	acknowledged_by, acknowledged_by_device, last_delivered_at, called_back_at,
	expired_at, canceled_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		in.ID, in.SendID, in.State, optStr(in.Tag), in.RetryCount,
		timeArg(in.ExpiresAt), timeArg(in.AcknowledgedAt),
		optStr(in.AcknowledgedBy), optStr(in.AcknowledgedByDevice),
		timeArg(in.LastDeliveredAt), timeArg(in.CalledBackAt),
		timeArg(in.ExpiredAt), timeArg(in.CanceledAt))
	return mapErr(err)
}

func (r *ReceiptRepo) GetByID(ctx context.Context, id string) (store.Receipt, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+receiptCols+` FROM receipts WHERE id = $1`, id)
	return scanReceipt(row)
}

// MarkLastDelivered sets last_delivered_at on a receipt while it is still NULL
// (first delivery only).
func (r *ReceiptRepo) MarkLastDelivered(ctx context.Context, receiptID string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE receipts SET last_delivered_at = $1 WHERE id = $2 AND last_delivered_at IS NULL`,
		at, receiptID); err != nil {
		return mapErr(err)
	}
	return nil
}

// receiptCols is the canonical receipt column list + scan order.
const receiptCols = `id, send_id, state, tag, retry_count, expires_at, acknowledged_at,
	acknowledged_by, acknowledged_by_device, last_delivered_at, called_back_at,
	expired_at, canceled_at`

func scanReceipt(s scanner) (store.Receipt, error) {
	var (
		rc         store.Receipt
		tag        sql.NullString
		expiresAt  sql.NullTime
		ackAt      sql.NullTime
		lastDeliv  sql.NullTime
		calledBack sql.NullTime
		expiredAt  sql.NullTime
		canceledAt sql.NullTime
		ackBy      sql.NullString
		ackByDev   sql.NullString
	)
	if err := s.Scan(&rc.ID, &rc.SendID, &rc.State, &tag, &rc.RetryCount, &expiresAt, &ackAt,
		&ackBy, &ackByDev, &lastDeliv, &calledBack, &expiredAt, &canceledAt); err != nil {
		return store.Receipt{}, mapErr(err)
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
	return rc, nil
}
