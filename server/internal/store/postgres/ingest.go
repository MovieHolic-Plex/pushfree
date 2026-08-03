package postgres

import (
	"context"

	"github.com/pushfree/pushfree/internal/store"
)

// IngestRepo is the Postgres implementation of store.IngestRepo. It performs
// the full atomic write for one POST /1/messages.json call: one sends row,
// per-recipient messages rows, an optional priority-2 receipt placeholder,
// and an optional attachment, all in a single transaction. It reuses the
// package-level insertSend / insertMessage / insertReceipt helpers and adds
// its own insertAttachment so the whole write shares one transaction.
type IngestRepo struct{ db DB }

// Ingest commits the send, its per-recipient message rows, an optional
// priority-2 receipt, and an optional attachment as one transaction. On any
// error the transaction is rolled back. The returned sendID is valid only
// when err == nil.
func (r *IngestRepo) Ingest(ctx context.Context, in *store.IngestInput) (int64, error) {
	var sendID int64
	err := inTx(ctx, r.db, func(q queryExec) error {
		if in.Receipt != nil {
			in.Send.ReceiptID = in.Receipt.ID
		}
		id, err := insertSend(ctx, q, &in.Send)
		if err != nil {
			return err
		}
		sendID = id
		for i := range in.Messages {
			m := in.Messages[i]
			m.SendID = id
			if _, err := insertMessage(ctx, q, &m); err != nil {
				return err
			}
		}
		if in.Receipt != nil {
			in.Receipt.SendID = id
			if err := insertReceipt(ctx, q, in.Receipt); err != nil {
				return err
			}
		}
		if in.Attachment != nil {
			in.Attachment.SendID = id
			if _, err := insertAttachment(ctx, q, in.Attachment); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return sendID, nil
}

// insertAttachment writes the 1:1 attachment row bound to sendID, accepting a
// queryExec so it runs inside the ingest transaction.
func insertAttachment(ctx context.Context, q queryExec, in *store.Attachment) (int64, error) {
	err := q.QueryRowContext(ctx,
		`INSERT INTO attachments(send_id, content_type, data, downloaded_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		in.SendID, in.ContentType, in.Data, timeArg(in.DownloadedAt)).Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}
