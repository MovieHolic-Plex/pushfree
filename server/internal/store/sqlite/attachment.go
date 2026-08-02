package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pushfree/pushfree/internal/store"
)

// AttachmentRepo is the SQLite implementation of store.AttachmentRepo.
type AttachmentRepo struct{ db DB }

func (a *AttachmentRepo) Create(ctx context.Context, in *store.Attachment) (int64, error) {
	res, err := a.db.ExecContext(ctx,
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

func (a *AttachmentRepo) GetBySendID(ctx context.Context, sendID int64) (store.Attachment, error) {
	var (
		att   store.Attachment
		dl    sql.NullString
	)
	err := a.db.QueryRowContext(ctx,
		`SELECT id, send_id, content_type, data, downloaded_at FROM attachments WHERE send_id = ?`,
		sendID,
	).Scan(&att.ID, &att.SendID, &att.ContentType, &att.Data, &dl)
	if err != nil {
		return store.Attachment{}, mapErr(err)
	}
	att.DownloadedAt = nullTime(dl)
	return att, nil
}
