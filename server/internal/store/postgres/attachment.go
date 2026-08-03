package postgres

import (
	"context"
	"database/sql"

	"github.com/pushfree/pushfree/internal/store"
)

// AttachmentRepo is the Postgres implementation of store.AttachmentRepo. The
// data column is BYTEA (binary), scanned into []byte directly.
type AttachmentRepo struct{ db DB }

// Create inserts an attachment row and writes the assigned id back to in.ID.
func (a *AttachmentRepo) Create(ctx context.Context, in *store.Attachment) (int64, error) {
	err := a.db.QueryRowContext(ctx,
		`INSERT INTO attachments(send_id, content_type, data, downloaded_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		in.SendID, in.ContentType, in.Data, timeArg(in.DownloadedAt)).Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

// GetBySendID loads the 1:1 attachment for a send.
func (a *AttachmentRepo) GetBySendID(ctx context.Context, sendID int64) (store.Attachment, error) {
	var (
		att store.Attachment
		dl  sql.NullTime
	)
	err := a.db.QueryRowContext(ctx,
		`SELECT id, send_id, content_type, data, downloaded_at FROM attachments WHERE send_id = $1`,
		sendID,
	).Scan(&att.ID, &att.SendID, &att.ContentType, &att.Data, &dl)
	if err != nil {
		return store.Attachment{}, mapErr(err)
	}
	att.DownloadedAt = nullTime(dl)
	return att, nil
}
