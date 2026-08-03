package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// SendRepo is the Postgres implementation of store.SendRepo. Its only
// multi-table operation is CreateFanout, which is transactional.
type SendRepo struct{ db DB }

// CreateFanout inserts one send, its per-recipient message rows, and an
// optional receipt as a single atomic transaction (H1 "sends-parent" model).
// On any error the transaction is rolled back: no send, no messages, and no
// receipt remain.
func (r *SendRepo) CreateFanout(ctx context.Context, f *store.Fanout) (int64, error) {
	var sendID int64
	err := inTx(ctx, r.db, func(q queryExec) error {
		if f.Receipt != nil {
			f.Send.ReceiptID = f.Receipt.ID
		}
		id, err := insertSend(ctx, q, &f.Send)
		if err != nil {
			return err
		}
		sendID = id
		for i := range f.Messages {
			m := f.Messages[i]
			m.SendID = id
			if _, err := insertMessage(ctx, q, &m); err != nil {
				return err
			}
		}
		if f.Receipt != nil {
			f.Receipt.SendID = id
			if err := insertReceipt(ctx, q, f.Receipt); err != nil {
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

func (r *SendRepo) GetByID(ctx context.Context, id int64) (store.Send, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+sendCols+` FROM sends WHERE id = $1`, id)
	return scanSend(row)
}

// ResolveRecipients is the SINGLE send-time lookup path for the "user" field
// (todo 9): it expands a list of 30-char keys into concrete recipient user
// IDs, resolving each as a user_key first, a group_key second, and a dynamic
// subscription key third. A key matching none of those returns ErrNotFound.
func (r *SendRepo) ResolveRecipients(ctx context.Context, keys []string) ([]int64, error) {
	var ids []int64
	for _, key := range keys {
		var userID int64
		err := r.db.QueryRowContext(ctx, `SELECT id FROM users WHERE user_key = $1`, key).Scan(&userID)
		if err == nil {
			ids = append(ids, userID)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, mapErr(err)
		}
		var groupID int64
		err = r.db.QueryRowContext(ctx, `SELECT id FROM groups WHERE group_key = $1`, key).Scan(&groupID)
		if errors.Is(err, sql.ErrNoRows) {
			var subUserID int64
			serr := r.db.QueryRowContext(ctx,
				`SELECT user_id FROM subscription_keys WHERE subscribed_key = $1`, key).Scan(&subUserID)
			if serr == nil {
				ids = append(ids, subUserID)
				continue
			}
			if errors.Is(serr, sql.ErrNoRows) {
				return nil, store.ErrNotFound
			}
			return nil, mapErr(serr)
		}
		if err != nil {
			return nil, mapErr(err)
		}
		rows, qerr := r.db.QueryContext(ctx,
			`SELECT user_id FROM group_members WHERE group_id = $1 ORDER BY user_id ASC`, groupID)
		if qerr != nil {
			return nil, mapErr(qerr)
		}
		for rows.Next() {
			var uid int64
			if serr := rows.Scan(&uid); serr != nil {
				rows.Close()
				return nil, mapErr(serr)
			}
			ids = append(ids, uid)
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return nil, mapErr(rerr)
		}
	}
	return ids, nil
}

// sendCols is the canonical send column list + scan order.
const sendCols = `id, app_id, sender_user_id, priority, sound, title, body, url, url_title,
	html, monospace, timestamp, ttl, tag, encrypted, callback_url, receipt_id, created_at`

func scanSend(s scanner) (store.Send, error) {
	var (
		sd      store.Send
		tag     sql.NullString
		cb      sql.NullString
		rid     sql.NullString
		created sql.NullTime
	)
	if err := s.Scan(&sd.ID, &sd.AppID, &sd.SenderUserID, &sd.Priority, &sd.Sound, &sd.Title,
		&sd.Body, &sd.URL, &sd.URLTitle, &sd.HTML, &sd.Monospace, &sd.Timestamp, &sd.TTL, &tag, &sd.Encrypted, &cb,
		&rid, &created); err != nil {
		return store.Send{}, mapErr(err)
	}
	sd.Tag = nullStr(tag)
	sd.CallbackURL = nullStr(cb)
	sd.ReceiptID = nullStr(rid)
	if created.Valid {
		sd.CreatedAt = created.Time
	}
	return sd, nil
}

// insertSend inserts a send row and writes the BIGSERIAL-assigned id back to
// in.ID via RETURNING (Postgres has no LastInsertId concept).
func insertSend(ctx context.Context, q queryExec, in *store.Send) (int64, error) {
	err := q.QueryRowContext(ctx, `
INSERT INTO sends(app_id, sender_user_id, priority, sound, title, body, url, url_title,
	html, monospace, timestamp, ttl, tag, encrypted, callback_url, receipt_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING id`,
		in.AppID, in.SenderUserID, in.Priority, in.Sound, in.Title, in.Body, in.URL, in.URLTitle,
		in.HTML, in.Monospace, in.Timestamp, in.TTL,
		optStr(in.Tag), in.Encrypted, optStr(in.CallbackURL), optStr(in.ReceiptID),
		in.CreatedAt).Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

// MessageRepo is the Postgres implementation of store.MessageRepo.
type MessageRepo struct{ db DB }

// Create inserts a single message row. Exposed for tests; the ingest path
// goes through SendRepo.CreateFanout or IngestRepo.Ingest.
func (m *MessageRepo) Create(ctx context.Context, in *store.Message) (int64, error) {
	return insertMessage(ctx, m.db, in)
}

func insertMessage(ctx context.Context, q queryExec, in *store.Message) (int64, error) {
	err := q.QueryRowContext(ctx, `
INSERT INTO messages(send_id, recipient_user_id, device_filter, delivered_at, created_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`,
		in.SendID, in.RecipientUserID, optStr(in.DeviceFilter), timeArg(in.DeliveredAt),
		in.CreatedAt).Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

// ListSince returns the per-recipient message rows with id > afterID, ordered
// by id ascending (the canonical delivery/replay cursor), capped at limit.
func (m *MessageRepo) ListSince(ctx context.Context, recipientUserID, afterID int64, limit int) ([]store.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := m.db.QueryContext(ctx, `
SELECT id, send_id, recipient_user_id, device_filter, delivered_at, created_at
FROM messages
WHERE recipient_user_id = $1 AND id > $2
ORDER BY id ASC
LIMIT $3`, recipientUserID, afterID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Message
	for rows.Next() {
		var (
			msg     store.Message
			filter  sql.NullString
			deliv   sql.NullTime
			created sql.NullTime
		)
		if err := rows.Scan(&msg.ID, &msg.SendID, &msg.RecipientUserID, &filter, &deliv, &created); err != nil {
			return nil, mapErr(err)
		}
		msg.DeviceFilter = nullStr(filter)
		msg.DeliveredAt = nullTime(deliv)
		if created.Valid {
			msg.CreatedAt = created.Time
		}
		out = append(out, msg)
	}
	return out, mapErr(rows.Err())
}

// MarkDelivered records the first transport-accepted delivery time on a
// message row. The `delivered_at IS NULL` guard makes it idempotent across
// replay/redelivery.
func (m *MessageRepo) MarkDelivered(ctx context.Context, messageID int64, at time.Time) error {
	if _, err := m.db.ExecContext(ctx,
		`UPDATE messages SET delivered_at = $1 WHERE id = $2 AND delivered_at IS NULL`,
		at, messageID); err != nil {
		return mapErr(err)
	}
	return nil
}

// MaxID returns the highest message id for a recipient, or 0 if the recipient
// has no messages.
func (m *MessageRepo) MaxID(ctx context.Context, recipientUserID int64) (int64, error) {
	var id sql.NullInt64
	err := m.db.QueryRowContext(ctx,
		`SELECT MAX(id) FROM messages WHERE recipient_user_id = $1`,
		recipientUserID).Scan(&id)
	if err != nil {
		return 0, mapErr(err)
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}
