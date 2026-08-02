package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// inTx runs fn inside a database transaction. If fn returns nil the
// transaction is committed; otherwise it is rolled back and fn's error is
// returned unwrapped-by-driver-but-mapped (see mapErr). The fn receives a
// queryExec bound to the transaction so all its statements share the same
// connection and atomicity boundary.
//
// SQLite (WAL, single writer) serializes transactions; busy_timeout=5000
// absorbs contention rather than returning SQLITE_BUSY.
func inTx(ctx context.Context, db DB, fn func(q queryExec) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	return fn(tx)
}

// Compile-time assertions that *sql.DB satisfies DB and *sql.Tx satisfies
// queryExec, guarding the abstraction as the codebase grows.
var (
	_ DB        = (*sql.DB)(nil)
	_ queryExec = (*sql.Tx)(nil)
	_ queryExec = (*sql.DB)(nil)
)
