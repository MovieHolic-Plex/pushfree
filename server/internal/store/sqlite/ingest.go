package sqlite

import (
	"context"
	"fmt"

	"github.com/pushfree/pushfree/internal/store"
)

// IngestRepo is the SQLite implementation of store.IngestRepo. It performs the
// full atomic write for one POST /1/messages.json call: one sends row,
// per-recipient messages rows, an optional priority-2 receipt placeholder, and
// an optional attachment, all in a single transaction.
//
// It is intentionally separate from SendRepo.CreateFanout (send_message.go) so
// the messages.json handler can persist the attachment atomically with the send
// without editing the worker-owned send_message.go/receipt.go files. It reuses
// the package-level insertSend / insertMessage / insertReceipt helpers (which
// are stable, committed code) and adds its own insertAttachment so the whole
// write shares one transaction and atomicity boundary.
type IngestRepo struct{ db DB }

// Ingest commits the send, its per-recipient message rows, an optional
// priority-2 receipt, and an optional attachment as one transaction (H1
// "sends-parent" model). On any error the transaction is rolled back: no send,
// no messages, no receipt, and no attachment remain. The returned sendID is
// valid only when err == nil.
func (r *IngestRepo) Ingest(ctx context.Context, in *store.IngestInput) (int64, error) {
	var sendID int64
	err := inTx(ctx, r.db, func(q queryExec) error {
		// The receipt belongs to this send (H1 1:1). Set the back-reference up
		// front so the send row carries its receipt_id in a single insert,
		// mirroring SendRepo.CreateFanout.
		if in.Receipt != nil {
			in.Send.ReceiptID = in.Receipt.ID
		}
		id, err := insertSend(ctx, q, &in.Send)
		if err != nil {
			return err
		}
		sendID = id
		for i := range in.Messages {
			// Mutate in-place so the DB-assigned id (set by insertMessage) is
			// visible to the caller through the *IngestInput pointer; the live
			// fan-out path publishes precisely these rows using their real ids
			// for subscribe-before-replay de-duplication.
			in.Messages[i].SendID = id
			if _, err := insertMessage(ctx, q, &in.Messages[i]); err != nil {
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

// insertAttachment writes the 1:1 attachment row bound to sendID. It mirrors
// AttachmentRepo.Create but accepts a queryExec so it runs inside the ingest
// transaction. Defined here (NEW in ingest.go) so attachment.go is untouched.
func insertAttachment(ctx context.Context, q queryExec, in *store.Attachment) (int64, error) {
	res, err := q.ExecContext(ctx,
		`INSERT INTO attachments(send_id, content_type, data, downloaded_at) VALUES (?, ?, ?, ?)`,
		in.SendID, in.ContentType, in.Data, nullTimePtr(in.DownloadedAt))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("attachment last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}
