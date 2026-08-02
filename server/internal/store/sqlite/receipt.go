package sqlite

import (
	"context"
	"database/sql"

	"github.com/pushfree/pushfree/internal/store"
)

// ReceiptRepo is the SQLite implementation of store.ReceiptRepo.
type ReceiptRepo struct{ db DB }

func (r *ReceiptRepo) Create(ctx context.Context, in *store.Receipt) error {
	return insertReceipt(ctx, r.db, in)
}

// insertReceipt inserts a receipt row. Enforces the receipt<->send 1:1 via
// the UNIQUE(receipts.send_id) constraint. Shared with the fan-out path.
func insertReceipt(ctx context.Context, q queryExec, in *store.Receipt) error {
	_, err := q.ExecContext(ctx, `
INSERT INTO receipts(id, send_id, state, tag, retry_count, expires_at, acknowledged_at,
	acknowledged_by, acknowledged_by_device, last_delivered_at, called_back_at,
	expired_at, canceled_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ID, in.SendID, in.State, optStr(in.Tag), in.RetryCount,
		nullTimePtr(in.ExpiresAt), nullTimePtr(in.AcknowledgedAt),
		optStr(in.AcknowledgedBy), optStr(in.AcknowledgedByDevice),
		nullTimePtr(in.LastDeliveredAt), nullTimePtr(in.CalledBackAt),
		nullTimePtr(in.ExpiredAt), nullTimePtr(in.CanceledAt))
	return mapErr(err)
}

func (r *ReceiptRepo) GetByID(ctx context.Context, id string) (store.Receipt, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+receiptCols+` FROM receipts WHERE id = ?`, id)
	return scanReceipt(row)
}

// receiptCols is the canonical receipt column list + scan order.
const receiptCols = `id, send_id, state, tag, retry_count, expires_at, acknowledged_at,
	acknowledged_by, acknowledged_by_device, last_delivered_at, called_back_at,
	expired_at, canceled_at`

func scanReceipt(s scanner) (store.Receipt, error) {
	var (
		rc  store.Receipt
		tag sql.NullString
		expiresAt,
		ackAt,
		lastDeliv,
		calledBack,
		expiredAt,
		canceledAt sql.NullString
		ackBy,
		ackByDev sql.NullString
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
