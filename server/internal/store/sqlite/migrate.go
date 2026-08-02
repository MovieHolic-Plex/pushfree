// Package sqlite is the pushfree SQLite repository implementation.
//
// It uses modernc.org/sqlite (pure Go, no cgo) so the server stays a single
// static binary. The migration framework is a minimal internal runner rather
// than golang-migrate: golang-migrate's sqlite database driver is built on
// mattn/go-sqlite3 (cgo), which is incompatible with the no-cgo requirement.
// The runner keeps golang-migrate's UX (numbered up/down files via embed.FS,
// schema_migrations table, idempotent Up/Down) without the dependency.
package sqlite

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationDir is the embed sub-directory holding the .sql files.
const migrationDir = "migrations"

var (
	migVersionRe = regexp.MustCompile(`^(\d+)_`)
	fileKindRe   = regexp.MustCompile(`\.(up|down)\.sql$`)
)

// migrationFile is one parsed .sql file.
type migrationFile struct {
	version int
	kind    string // "up" | "down"
	name    string // full file name
	source  string // SQL body
}

// loadMigrations reads and parses the embedded migration files, returning up
// files and down files each sorted ascending by version.
func loadMigrations() (ups, downs []migrationFile, err error) {
	entries, err := fs.ReadDir(migrationFS, migrationDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		kind := fileKindRe.FindStringSubmatch(e.Name())
		if len(kind) != 2 {
			return nil, nil, fmt.Errorf("migration %q: name must match <n>_name.(up|down).sql", e.Name())
		}
		ver := migVersionRe.FindStringSubmatch(e.Name())
		if len(ver) != 2 {
			return nil, nil, fmt.Errorf("migration %q: name must start with a numeric version", e.Name())
		}
		n, perr := strconv.Atoi(ver[1])
		if perr != nil {
			return nil, nil, fmt.Errorf("migration %q: parse version: %w", e.Name(), perr)
		}
		body, rerr := migrationFS.ReadFile(migrationDir + "/" + e.Name())
		if rerr != nil {
			return nil, nil, fmt.Errorf("read migration %q: %w", e.Name(), rerr)
		}
		mf := migrationFile{version: n, kind: kind[1], name: e.Name(), source: string(body)}
		switch mf.kind {
		case "up":
			ups = append(ups, mf)
		case "down":
			downs = append(downs, mf)
		}
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].version < ups[j].version })
	sort.Slice(downs, func(i, j int) bool { return downs[i].version < downs[j].version })
	return ups, downs, nil
}

const ensureSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    dirty      INTEGER NOT NULL DEFAULT 0,
    applied_at TEXT NOT NULL
)`

// appliedVersions returns the set of applied migration versions in ascending
// order. It lazily ensures schema_migrations exists so Up is safe on a fresh
// database.
func appliedVersions(ctx context.Context, q ctxExec) ([]int, error) {
	if _, err := q.ExecContext(ctx, ensureSchemaMigrations); err != nil {
		return nil, fmt.Errorf("ensure schema_migrations: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT version FROM schema_migrations WHERE dirty = 0 ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// runOne applies a single migration file inside a transaction. It records the
// version (dirty=0) on success. If the SQL fails the transaction rolls back
// and the version is marked dirty=1 so a human must intervene, matching
// golang-migrate semantics.
func runOne(ctx context.Context, db ctxExecBegin, mf migrationFile, markApplied bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx for %s: %w", mf.name, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed.

	if _, err := tx.ExecContext(ctx, mf.source); err != nil {
		// Mark dirty so a re-run does not silently skip a broken migration.
		_, _ = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, dirty, applied_at) VALUES (?, 1, ?)
			ON CONFLICT(version) DO UPDATE SET dirty=1, applied_at=excluded.applied_at`, mf.version, time.Now().UTC().Format(time.RFC3339))
		return fmt.Errorf("apply %s: %w", mf.name, err)
	}
	if markApplied {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, dirty, applied_at) VALUES (?, 0, ?)
			 ON CONFLICT(version) DO UPDATE SET dirty=0, applied_at=excluded.applied_at`,
			mf.version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("record %s: %w", mf.name, err)
		}
	} else {
		// Down: remove the version marker.
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, mf.version); err != nil {
			return fmt.Errorf("forget %s: %w", mf.name, err)
		}
	}
	return tx.Commit()
}

// Up applies every not-yet-applied up migration in ascending version order.
// It is idempotent: running it on an up-to-date database is a no-op.
func Up(ctx context.Context, db DB) error {
	ups, _, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	seen := make(map[int]bool, len(applied))
	for _, v := range applied {
		seen[v] = true
	}
	for _, mf := range ups {
		if seen[mf.version] {
			continue
		}
		if err := runOne(ctx, db, mf, true); err != nil {
			return err
		}
	}
	return nil
}

// Down reverts every applied migration in descending version order, leaving
// the database schema-empty (schema_migrations itself is dropped last).
// Idempotent: on a schema-empty database it is a no-op.
func Down(ctx context.Context, db DB) error {
	_, downs, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	seen := make(map[int]bool, len(applied))
	for _, v := range applied {
		seen[v] = true
	}
	// Descending so the newest schema is reverted first.
	for i := len(downs) - 1; i >= 0; i-- {
		mf := downs[i]
		if !seen[mf.version] {
			continue
		}
		if err := runOne(ctx, db, mf, false); err != nil {
			return err
		}
	}
	// Drop the bookkeeping table itself; safe no-op if absent.
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS schema_migrations`)
	return nil
}

// Version reports the highest applied migration version, or 0 if none.
func Version(ctx context.Context, db DB) (int, error) {
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return 0, err
	}
	if len(applied) == 0 {
		return 0, nil
	}
	return applied[len(applied)-1], nil
}
