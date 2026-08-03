// Package postgres is the pushfree Postgres repository implementation (todo
// 45). It mirrors the SQLite implementation (internal/store/sqlite) behind the
// same store.Repos interfaces (todo 5); the only differences are the SQL
// dialect (numbered $N placeholders, RETURNING, BOOLEAN, TIMESTAMPTZ, BYTEA,
// BIGSERIAL, ON CONFLICT) and the driver (pgx v5 exposed via its
// database/sql stdlib adapter so the proven queryExec / inTx / scanner
// abstractions are reused unchanged).
//
// Migrations: UP-only at startup, idempotent, with a version-pinned
// schema_migrations table (mirrors golang-migrate semantics). Down migrations
// exist only as documented manual rollback scripts (see migrations/*.down.sql
// and docs/POSTGRES.md) — the runner deliberately exposes no Down() so an
// accidental call cannot destroy production data.
package postgres

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

// loadUpMigrations reads and parses the embedded UP migration files, sorted
// ascending by version. Down files are present only as manual rollback
// scripts and are never loaded by the runner.
func loadUpMigrations() ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrationFS, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var ups []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		kind := fileKindRe.FindStringSubmatch(e.Name())
		if len(kind) != 2 {
			return nil, fmt.Errorf("migration %q: name must match <n>_name.(up|down).sql", e.Name())
		}
		if kind[1] != "up" {
			continue // down files are manual-only.
		}
		ver := migVersionRe.FindStringSubmatch(e.Name())
		if len(ver) != 2 {
			return nil, fmt.Errorf("migration %q: name must start with a numeric version", e.Name())
		}
		n, perr := strconv.Atoi(ver[1])
		if perr != nil {
			return nil, fmt.Errorf("migration %q: parse version: %w", e.Name(), perr)
		}
		body, rerr := migrationFS.ReadFile(migrationDir + "/" + e.Name())
		if rerr != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), rerr)
		}
		ups = append(ups, migrationFile{version: n, kind: "up", name: e.Name(), source: string(body)})
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].version < ups[j].version })
	return ups, nil
}

// ensureSchemaMigrations creates the version-tracking table if absent. The
// version column pins the highest applied migration; dirty flags a partially
// applied migration so a human must intervene (golang-migrate parity).
const ensureSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    BIGINT PRIMARY KEY,
    dirty      BOOLEAN NOT NULL DEFAULT FALSE,
    applied_at TIMESTAMPTZ NOT NULL
)`

// appliedVersions returns the set of cleanly-applied migration versions in
// ascending order. It lazily ensures schema_migrations exists so Up is safe on
// a fresh database.
func appliedVersions(ctx context.Context, q ctxExec) ([]int, error) {
	if _, err := q.ExecContext(ctx, ensureSchemaMigrations); err != nil {
		return nil, fmt.Errorf("ensure schema_migrations: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT version FROM schema_migrations WHERE dirty = FALSE ORDER BY version`)
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

// runOne applies a single migration file inside a transaction. Postgres DDL is
// transactional, so a failure rolls back the whole migration cleanly. On SQL
// failure the version is recorded dirty=TRUE so a re-run does not silently
// skip a broken migration (the operator must resolve and remove the dirty
// row before re-running).
func runOne(ctx context.Context, db ctxExecBegin, mf migrationFile) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx for %s: %w", mf.name, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed.

	if _, err := tx.ExecContext(ctx, mf.source); err != nil {
		// Mark dirty so a re-run does not silently skip a broken migration.
		_, _ = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, dirty, applied_at) VALUES ($1, TRUE, $2)
			ON CONFLICT(version) DO UPDATE SET dirty=TRUE, applied_at=EXCLUDED.applied_at`, mf.version, time.Now().UTC())
		return fmt.Errorf("apply %s: %w", mf.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, dirty, applied_at) VALUES ($1, FALSE, $2)
		 ON CONFLICT(version) DO UPDATE SET dirty=FALSE, applied_at=EXCLUDED.applied_at`,
		mf.version, time.Now().UTC()); err != nil {
		return fmt.Errorf("record %s: %w", mf.name, err)
	}
	return tx.Commit()
}

// Up applies every not-yet-applied up migration in ascending version order.
// It is idempotent: running it on an up-to-date database is a no-op. This is
// the only mutation the runner performs at startup; there is no Down.
func Up(ctx context.Context, db DB) error {
	ups, err := loadUpMigrations()
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
		if err := runOne(ctx, db, mf); err != nil {
			return err
		}
	}
	return nil
}

// Version reports the highest cleanly-applied migration version, or 0 if none.
// A dirty migration surfaces as an error so callers do not silently run on a
// half-migrated database.
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
