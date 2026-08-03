package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// SendRepo is the SQLite implementation of store.SendRepo. Its only
// multi-table operation is CreateFanout, which is transactional.
type SendRepo struct{ db DB }

// CreateFanout inserts one send, its per-recipient message rows, and an
// optional receipt as a single atomic transaction (H1 "sends-parent" model).
// On any error the transaction is rolled back: no send, no messages, and no
// receipt remain. The returned sendID is valid only when err == nil.
func (r *SendRepo) CreateFanout(ctx context.Context, f *store.Fanout) (int64, error) {
	var sendID int64
	err := inTx(ctx, r.db, func(q queryExec) error {
		if f.Receipt != nil {
			// The receipt belongs to this send (H1 1:1). Set the
			// back-reference up front so the send row carries its
			// receipt_id in a single insert.
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
	row := r.db.QueryRowContext(ctx, `SELECT `+sendCols+` FROM sends WHERE id = ?`, id)
	return scanSend(row)
}

// ResolveRecipients is the SINGLE send-time lookup path for the "user" field
// (todo 9): it expands a list of 30-char keys into concrete recipient user
// IDs, resolving each key as a user_key first and a group_key second. The
// caller cannot tell the two key kinds apart, so this method is the only place
// that decides.
//
//   - A key matching a users.user_key row contributes that one user id.
//   - A key matching a groups.group_key row contributes every member user id.
//   - A key matching a subscription_keys.subscribed_key row (todo 12: a
//     per-(app, user) dynamic subscriber key) contributes its underlying
//     user id, transparently like a user_key.
//   - A key matching NONE of the above returns ErrNotFound (the API maps this
//     to 404).
//
// A group with zero members contributes nothing but is NOT an error (the key
// resolved to a known group); the overall result may then be empty.
func (r *SendRepo) ResolveRecipients(ctx context.Context, keys []string) ([]int64, error) {
	var ids []int64
	for _, key := range keys {
		// Try a user first: the overwhelmingly common case, and a single-row
		// lookup that avoids touching group_members at all.
		var userID int64
		err := r.db.QueryRowContext(ctx, `SELECT id FROM users WHERE user_key = ?`, key).Scan(&userID)
		if err == nil {
			ids = append(ids, userID)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, mapErr(err)
		}
		// Not a user. Resolve as a group, expanding its members.
		var groupID int64
		err = r.db.QueryRowContext(ctx, `SELECT id FROM groups WHERE group_key = ?`, key).Scan(&groupID)
		if errors.Is(err, sql.ErrNoRows) {
			// Not a group either. Try a dynamic subscriber key (todo 12): a
			// subscription approval mints a per-(app, user) key that resolves
			// to its underlying user_id, behaving like a user_key.
			var subUserID int64
			serr := r.db.QueryRowContext(ctx,
				`SELECT user_id FROM subscription_keys WHERE subscribed_key = ?`, key).Scan(&subUserID)
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
			`SELECT user_id FROM group_members WHERE group_id = ? ORDER BY user_id ASC`, groupID)
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
		html    int64
		mono    int64
		enc     int64
		tag     sql.NullString
		cb      sql.NullString
		rid     sql.NullString
		created sql.NullString
	)
	if err := s.Scan(&sd.ID, &sd.AppID, &sd.SenderUserID, &sd.Priority, &sd.Sound, &sd.Title,
		&sd.Body, &sd.URL, &sd.URLTitle, &html, &mono, &sd.Timestamp, &sd.TTL, &tag, &enc, &cb,
		&rid, &created); err != nil {
		return store.Send{}, mapErr(err)
	}
	sd.HTML = html != 0
	sd.Monospace = mono != 0
	sd.Encrypted = enc != 0
	sd.Tag = nullStr(tag)
	sd.CallbackURL = nullStr(cb)
	sd.ReceiptID = nullStr(rid)
	if t, ok := parseTime(created.String, created.Valid); ok {
		sd.CreatedAt = t
	}
	return sd, nil
}

// scanner is the shared Scan surface of *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func insertSend(ctx context.Context, q queryExec, in *store.Send) (int64, error) {
	res, err := q.ExecContext(ctx, `
INSERT INTO sends(app_id, sender_user_id, priority, sound, title, body, url, url_title,
	html, monospace, timestamp, ttl, tag, encrypted, callback_url, receipt_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.AppID, in.SenderUserID, in.Priority, in.Sound, in.Title, in.Body, in.URL, in.URLTitle,
		boolToInt(in.HTML), boolToInt(in.Monospace), in.Timestamp, in.TTL,
		optStr(in.Tag), boolToInt(in.Encrypted), optStr(in.CallbackURL), optStr(in.ReceiptID),
		rfc3339(in.CreatedAt))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("send last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// MessageRepo is the SQLite implementation of store.MessageRepo.
type MessageRepo struct{ db DB }

// Create inserts a single message row. Exposed for tests; the ingest path
// goes through SendRepo.CreateFanout.
func (m *MessageRepo) Create(ctx context.Context, in *store.Message) (int64, error) {
	return insertMessage(ctx, m.db, in)
}

func insertMessage(ctx context.Context, q queryExec, in *store.Message) (int64, error) {
	res, err := q.ExecContext(ctx, `
INSERT INTO messages(send_id, recipient_user_id, device_filter, delivered_at, created_at)
VALUES (?, ?, ?, ?, ?)`,
		in.SendID, in.RecipientUserID, optStr(in.DeviceFilter), nullTimePtr(in.DeliveredAt),
		rfc3339(in.CreatedAt))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("message last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

// ListSince returns the per-recipient message rows with id > afterID, ordered
// by id ascending (the canonical delivery/replay cursor), capped at limit.
// It backs the WS/SSE "since" replay (todo 13).
func (m *MessageRepo) ListSince(ctx context.Context, recipientUserID, afterID int64, limit int) ([]store.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := m.db.QueryContext(ctx, `
SELECT id, send_id, recipient_user_id, device_filter, delivered_at, created_at
FROM messages
WHERE recipient_user_id = ? AND id > ?
ORDER BY id ASC
LIMIT ?`, recipientUserID, afterID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Message
	for rows.Next() {
		var (
			msg     store.Message
			filter  sql.NullString
			deliv   sql.NullString
			created sql.NullString
		)
		if err := rows.Scan(&msg.ID, &msg.SendID, &msg.RecipientUserID, &filter, &deliv, &created); err != nil {
			return nil, mapErr(err)
		}
		msg.DeviceFilter = nullStr(filter)
		msg.DeliveredAt = nullTime(deliv)
		if t, ok := parseTime(created.String, created.Valid); ok {
			msg.CreatedAt = t
		}
		out = append(out, msg)
	}
	return out, mapErr(rows.Err())
}

// MarkDelivered records the first transport-accepted delivery time on a
// message row. The `delivered_at IS NULL` guard makes it idempotent across
// replay/redelivery: only the first accepted write sets the timestamp.
func (m *MessageRepo) MarkDelivered(ctx context.Context, messageID int64, at time.Time) error {
	if _, err := m.db.ExecContext(ctx,
		`UPDATE messages SET delivered_at = ? WHERE id = ? AND delivered_at IS NULL`,
		rfc3339(at), messageID); err != nil {
		return mapErr(err)
	}
	return nil
}

// MaxID returns the highest message id for a recipient, or 0 if the recipient
// has no messages. MAX returns NULL over an empty set, scanned into NullInt64.
func (m *MessageRepo) MaxID(ctx context.Context, recipientUserID int64) (int64, error) {
	var id sql.NullInt64
	err := m.db.QueryRowContext(ctx,
		`SELECT MAX(id) FROM messages WHERE recipient_user_id = ?`,
		recipientUserID).Scan(&id)
	if err != nil {
		return 0, mapErr(err)
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}
