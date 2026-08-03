package postgres

import (
	"context"
	"database/sql"

	"github.com/pushfree/pushfree/internal/store"
)

// UserRepo is the Postgres implementation of store.UserRepo.
type UserRepo struct{ db DB }

// Create inserts a user and writes the assigned id back to in.ID.
func (u *UserRepo) Create(ctx context.Context, in *store.User) (int64, error) {
	return createUser(ctx, u.db, in)
}

// CreateBootstrap inserts a user, becoming admin iff it is the first row. The
// role is decided by a CASE over a count(*) subquery inside the INSERT;
// Postgres executes it under the row lock, and the UNIQUE(user_key) constraint
// serializes concurrent first-time registrations so only one becomes admin
// (the other hits a constraint failure on the user_key it independently
// generated — the API layer regenerates and retries).
func createUser(ctx context.Context, q queryExec, in *store.User) (int64, error) {
	err := q.QueryRowContext(ctx, `
INSERT INTO users(email, pass_hash, role, user_key, quiet_start, quiet_end, quiet_tz, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`,
		in.Email, in.PassHash, in.Role, in.UserKey,
		optStr(in.QuietStart), optStr(in.QuietEnd), defaultIfEmpty(in.QuietTZ, "UTC"),
		in.CreatedAt).Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

func (u *UserRepo) GetByID(ctx context.Context, id int64) (store.User, error) {
	return getUser(ctx, u.db, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
}

func (u *UserRepo) GetByEmail(ctx context.Context, email string) (store.User, error) {
	return getUser(ctx, u.db, `SELECT `+userCols+` FROM users WHERE email = $1`, email)
}

func (u *UserRepo) GetByUserKey(ctx context.Context, key string) (store.User, error) {
	return getUser(ctx, u.db, `SELECT `+userCols+` FROM users WHERE user_key = $1`, key)
}

// CreateBootstrap mirrors the SQLite CASE-over-count bootstrap: the first user
// row becomes admin, every later one becomes a regular user, decided inside
// the same INSERT that stores the row.
func (u *UserRepo) CreateBootstrap(ctx context.Context, in *store.User) (int64, error) {
	err := u.db.QueryRowContext(ctx, `
INSERT INTO users(email, pass_hash, role, user_key, quiet_start, quiet_end, quiet_tz, created_at)
SELECT $1, $2,
       CASE WHEN (SELECT count(*) FROM users) = 0 THEN 'admin' ELSE 'user' END,
       $3, $4, $5, $6, $7
RETURNING id, role`,
		in.Email, in.PassHash, in.UserKey,
		optStr(in.QuietStart), optStr(in.QuietEnd), defaultIfEmpty(in.QuietTZ, "UTC"),
		in.CreatedAt).Scan(&in.ID, &in.Role)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

// UpdateQuietHours persists a user's quiet-hours window. Pass "" for
// quietStart/quietEnd to clear the window (NULL). Returns ErrNotFound if id
// is absent.
func (u *UserRepo) UpdateQuietHours(ctx context.Context, id int64, quietStart, quietEnd, tz string) error {
	res, err := u.db.ExecContext(ctx,
		`UPDATE users SET quiet_start = $1, quiet_end = $2, quiet_tz = $3 WHERE id = $4`,
		optStr(quietStart), optStr(quietEnd), defaultIfEmpty(tz, "UTC"), id)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// userCols is the canonical column list + scan order for a user row.
const userCols = `id, email, pass_hash, role, user_key, quiet_start, quiet_end, quiet_tz, created_at`

func scanUser(s scanner) (store.User, error) {
	var u store.User
	var quietStart, quietEnd, tz sql.NullString
	var created sql.NullTime
	if err := s.Scan(&u.ID, &u.Email, &u.PassHash, &u.Role, &u.UserKey, &quietStart, &quietEnd, &tz, &created); err != nil {
		return store.User{}, mapErr(err)
	}
	u.QuietStart = nullStr(quietStart)
	u.QuietEnd = nullStr(quietEnd)
	u.QuietTZ = defaultIfEmpty(nullStr(tz), "UTC")
	if created.Valid {
		u.CreatedAt = created.Time
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

// defaultIfEmpty returns def when s is empty, else s.
func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
