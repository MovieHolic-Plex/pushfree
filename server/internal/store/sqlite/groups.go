package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pushfree/pushfree/internal/store"
)

// GroupRepo is the SQLite implementation of store.GroupRepo (todo 9). Group
// rows live in `groups`; membership is the composite-PK `group_members` table.
// Member identities are user_ids at this layer; the API handler resolves
// user_key <-> user_id.
type GroupRepo struct{ db DB }

// groupCols is the canonical groups column list + scan order.
const groupCols = `id, user_id, group_key, name, memo, created_at`

// Create inserts a group row. The caller must set UserID, GroupKey, Name,
// Memo, and CreatedAt; the assigned ID is written back to g.
func (r *GroupRepo) Create(ctx context.Context, g *store.Group) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
INSERT INTO groups(user_id, group_key, name, memo, created_at)
VALUES (?, ?, ?, ?, ?)`,
		g.UserID, g.GroupKey, g.Name, g.Memo, rfc3339(g.CreatedAt))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("group last insert id: %w", err)
	}
	g.ID = id
	return id, nil
}

func (r *GroupRepo) GetByID(ctx context.Context, id int64) (store.Group, error) {
	return scanGroupRow(ctx, r.db, `SELECT `+groupCols+` FROM groups WHERE id = ?`, id)
}

func (r *GroupRepo) GetByKey(ctx context.Context, key string) (store.Group, error) {
	return scanGroupRow(ctx, r.db, `SELECT `+groupCols+` FROM groups WHERE group_key = ?`, key)
}

// ListByOwner returns every group owned by ownerID, ordered by id ascending
// (creation order), for GET /1/groups.json.
func (r *GroupRepo) ListByOwner(ctx context.Context, ownerID int64) ([]store.Group, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+groupCols+` FROM groups WHERE user_id = ? ORDER BY id ASC`, ownerID)
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
		`UPDATE groups SET name = ?, memo = ? WHERE id = ?`, name, memo, id)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Delete removes the group and its members in one transaction. Members are
// deleted first (FK -> groups); the group row goes last.
func (r *GroupRepo) Delete(ctx context.Context, id int64) error {
	err := inTx(ctx, r.db, func(q queryExec) error {
		if _, err := q.ExecContext(ctx, `DELETE FROM group_members WHERE group_id = ?`, id); err != nil {
			return mapErr(err)
		}
		res, err := q.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id)
		if err != nil {
			return mapErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		return nil
	})
	return err
}

// SetMembers adds and removes members (by user_id) in one transaction. Adds
// use INSERT OR IGNORE so a duplicate add is a no-op; removes of a non-member
// are a no-op. The whole operation is atomic: a partial membership change
// never commits.
func (r *GroupRepo) SetMembers(ctx context.Context, groupID int64, add, remove []int64) error {
	return inTx(ctx, r.db, func(q queryExec) error {
		for _, uid := range add {
			if _, err := q.ExecContext(ctx,
				`INSERT OR IGNORE INTO group_members(group_id, user_id) VALUES (?, ?)`,
				groupID, uid); err != nil {
				return mapErr(err)
			}
		}
		for _, uid := range remove {
			if _, err := q.ExecContext(ctx,
				`DELETE FROM group_members WHERE group_id = ? AND user_id = ?`,
				groupID, uid); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

// ListMemberIDs returns the user_ids of the members of groupID, ordered by
// user_id ascending for deterministic fan-out ordering.
func (r *GroupRepo) ListMemberIDs(ctx context.Context, groupID int64) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id FROM group_members WHERE group_id = ? ORDER BY user_id ASC`, groupID)
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
// users), for surfacing membership in the groups API response. Ordered by
// user_id ascending to match ListMemberIDs.
func (r *GroupRepo) ListMemberKeys(ctx context.Context, groupID int64) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT u.user_key FROM group_members gm
JOIN users u ON u.id = gm.user_id
WHERE gm.group_id = ?
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

// scanGroup scans a group row from a *sql.Rows (the multi-row scanner).
func scanGroup(rows *sql.Rows) (store.Group, error) {
	var (
		g       store.Group
		created sql.NullString
	)
	if err := rows.Scan(&g.ID, &g.UserID, &g.GroupKey, &g.Name, &g.Memo, &created); err != nil {
		return store.Group{}, mapErr(err)
	}
	if t, ok := parseTime(created.String, created.Valid); ok {
		g.CreatedAt = t
	}
	return g, nil
}

// scanGroupRow scans a single group row from a *sql.Row query.
func scanGroupRow(ctx context.Context, q queryExec, query string, args ...any) (store.Group, error) {
	var (
		g       store.Group
		created sql.NullString
	)
	err := q.QueryRowContext(ctx, query, args...).
		Scan(&g.ID, &g.UserID, &g.GroupKey, &g.Name, &g.Memo, &created)
	if err != nil {
		return store.Group{}, mapErr(err)
	}
	if t, ok := parseTime(created.String, created.Valid); ok {
		g.CreatedAt = t
	}
	return g, nil
}
