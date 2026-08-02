package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pushfree/pushfree/internal/store"
)

// UserRepo is the SQLite implementation of store.UserRepo.
type UserRepo struct{ db DB }

func (u *UserRepo) Create(ctx context.Context, in *store.User) (int64, error) {
	return createUser(ctx, u.db, in)
}

func createUser(ctx context.Context, q queryExec, in *store.User) (int64, error) {
	res, err := q.ExecContext(ctx, `
INSERT INTO users(email, pass_hash, role, user_key, quiet_start, quiet_end, quiet_tz, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Email, in.PassHash, in.Role, in.UserKey,
		optStr(in.QuietStart), optStr(in.QuietEnd), defaultIfEmpty(in.QuietTZ, "UTC"),
		rfc3339(in.CreatedAt))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("user last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

func (u *UserRepo) GetByID(ctx context.Context, id int64) (store.User, error) {
	return getUser(ctx, u.db, `SELECT `+userCols+` FROM users WHERE id = ?`, id)
}

func (u *UserRepo) GetByEmail(ctx context.Context, email string) (store.User, error) {
	return getUser(ctx, u.db, `SELECT `+userCols+` FROM users WHERE email = ?`, email)
}

func (u *UserRepo) GetByUserKey(ctx context.Context, key string) (store.User, error) {
	return getUser(ctx, u.db, `SELECT `+userCols+` FROM users WHERE user_key = ?`, key)
}

// userCols is the canonical column list + scan order for a user row. Kept in
// one place so SELECTs and Scan stay in sync.
const userCols = `id, email, pass_hash, role, user_key, quiet_start, quiet_end, quiet_tz, created_at`

func scanUser(s interface {
	Scan(dest ...any) error
}) (store.User, error) {
	var u store.User
	var quietStart, quietEnd, tz, created sql.NullString
	if err := s.Scan(&u.ID, &u.Email, &u.PassHash, &u.Role, &u.UserKey, &quietStart, &quietEnd, &tz, &created); err != nil {
		return store.User{}, mapErr(err)
	}
	u.QuietStart = nullStr(quietStart)
	u.QuietEnd = nullStr(quietEnd)
	u.QuietTZ = defaultIfEmpty(nullStr(tz), "UTC")
	if t, ok := parseTime(created.String, created.Valid); ok {
		u.CreatedAt = t
	}
	return u, nil
}

func getUser(ctx context.Context, q queryExec, query string, args ...any) (store.User, error) {
	row := q.QueryRowContext(ctx, query, args...)
	u, err := scanUser(row)
	if err != nil {
		return store.User{}, err
	}
	return u, nil
}

// optStr returns NULL for "" so optional TEXT columns store NULL rather than
// the empty string (lets CHECK ... IS NULL and partial indexes work).
func optStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
