package postgres

import (
	"context"
	"database/sql"

	"github.com/pushfree/pushfree/internal/store"
)

// GroupRepo is the Postgres implementation of store.GroupRepo (todo 9). Group
// rows live in `groups`; membership is the composite-PK `group_members` table.
type GroupRepo struct{ db DB }

// groupCols is the canonical groups column list + scan order.
const groupCols = `id, user_id, group_key, name, memo, created_at`

// Create inserts a group row and writes the assigned id back to g.
func (r *GroupRepo) Create(ctx context.Context, g *store.Group) (int64, error) {
	err := r.db.QueryRowContext(ctx, `
INSERT INTO groups(user_id, group_key, name, memo, created_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`,
		g.UserID, g.GroupKey, g.Name, g.Memo, g.CreatedAt).Scan(&g.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return g.ID, nil
}

func (r *GroupRepo) GetByID(ctx context.Context, id int64) (store.Group, error) {
	return scanGroupRow(ctx, r.db, `SELECT `+groupCols+` FROM groups WHERE id = $1`, id)
}

func (r *GroupRepo) GetByKey(ctx context.Context, key string) (store.Group, error) {
	return scanGroupRow(ctx, r.db, `SELECT `+groupCols+` FROM groups WHERE group_key = $1`, key)
}

// ListByOwner returns every group owned by ownerID, ordered by id ascending.
func (r *GroupRepo) ListByOwner(ctx context.Context, ownerID int64) ([]store.Group, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+groupCols+` FROM groups WHERE user_id = $1 ORDER BY id ASC`, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, mapErr(rows.Err())
}

// Update changes the name and memo of the group with the given id. A zero
// rows-affected result means the id is absent -> ErrNotFound.
func (r *GroupRepo) Update(ctx context.Context, id int64, name, memo string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE groups SET name = $1, memo = $2 WHERE id = $3`, name, memo, id)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Delete removes the group and its members in one transaction.
func (r *GroupRepo) Delete(ctx context.Context, id int64) error {
	return inTx(ctx, r.db, func(q queryExec) error {
		if _, err := q.ExecContext(ctx, `DELETE FROM group_members WHERE group_id = $1`, id); err != nil {
			return mapErr(err)
		}
		res, err := q.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, id)
		if err != nil {
			return mapErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		return nil
	})
}

// SetMembers adds and removes members (by user_id) in one transaction. Adds
// use ON CONFLICT DO NOTHING so a duplicate add is a no-op; removes of a
// non-member are a no-op.
func (r *GroupRepo) SetMembers(ctx context.Context, groupID int64, add, remove []int64) error {
	return inTx(ctx, r.db, func(q queryExec) error {
		for _, uid := range add {
			if _, err := q.ExecContext(ctx,
				`INSERT INTO group_members(group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
				groupID, uid); err != nil {
				return mapErr(err)
			}
		}
		for _, uid := range remove {
			if _, err := q.ExecContext(ctx,
				`DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`,
				groupID, uid); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

// ListMemberIDs returns the user_ids of the members of groupID, ordered by
// user_id ascending.
func (r *GroupRepo) ListMemberIDs(ctx context.Context, groupID int64) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id FROM group_members WHERE group_id = $1 ORDER BY user_id ASC`, groupID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, mapErr(err)
		}
		ids = append(ids, uid)
	}
	return ids, mapErr(rows.Err())
}

// ListMemberKeys returns the user_keys of the members of groupID (joined to
// users), ordered by user_id ascending.
func (r *GroupRepo) ListMemberKeys(ctx context.Context, groupID int64) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT u.user_key FROM group_members gm
JOIN users u ON u.id = gm.user_id
WHERE gm.group_id = $1
ORDER BY gm.user_id ASC`, groupID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, mapErr(err)
		}
		keys = append(keys, k)
	}
	return keys, mapErr(rows.Err())
}

func scanGroup(rows *sql.Rows) (store.Group, error) {
	var (
		g       store.Group
		created sql.NullTime
	)
	if err := rows.Scan(&g.ID, &g.UserID, &g.GroupKey, &g.Name, &g.Memo, &created); err != nil {
		return store.Group{}, mapErr(err)
	}
	if created.Valid {
		g.CreatedAt = created.Time
	}
	return g, nil
}

func scanGroupRow(ctx context.Context, q queryExec, query string, args ...any) (store.Group, error) {
	var (
		g       store.Group
		created sql.NullTime
	)
	err := q.QueryRowContext(ctx, query, args...).
		Scan(&g.ID, &g.UserID, &g.GroupKey, &g.Name, &g.Memo, &created)
	if err != nil {
		return store.Group{}, mapErr(err)
	}
	if created.Valid {
		g.CreatedAt = created.Time
	}
	return g, nil
}
