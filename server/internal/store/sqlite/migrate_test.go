package sqlite

import (
	"context"
	"testing"
)

// TestMigrationsUpDownUp verifies the migration runner is idempotent: a fresh
// DB can go up, fully down, and back up, ending in a usable schema with the
// expected tables present, then absent, then present again. This is the
// mandatory up/down coverage required by the plan.
func TestMigrationsUpDownUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newRawDB(t)

	wantTables := []string{
		"users", "apps", "devices", "sends", "messages", "attachments",
		"receipts", "quota_counters", "timers", "callbacks", "dlq",
		"groups", "group_members",
	}

	// Up on a fresh DB applies every migration and creates every table.
	if err := Up(ctx, s.DB()); err != nil {
		t.Fatalf("Up #1: %v", err)
	}
	v, err := Version(ctx, s.DB())
	if err != nil {
		t.Fatalf("Version after Up #1: %v", err)
	}
	if v != 2 {
		t.Fatalf("version after Up #1 = %d, want 2", v)
	}
	for _, tbl := range wantTables {
		if !tableExists(t, s, tbl) {
			t.Fatalf("after Up #1: table %q missing", tbl)
		}
	}

	// Up again is a no-op (idempotent).
	if err := Up(ctx, s.DB()); err != nil {
		t.Fatalf("Up #1b (idempotent): %v", err)
	}

	// Down reverts everything; tables are gone and version resets to 0.
	if err := Down(ctx, s.DB()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	v, err = Version(ctx, s.DB())
	if err != nil {
		t.Fatalf("Version after Down: %v", err)
	}
	if v != 0 {
		t.Fatalf("version after Down = %d, want 0", v)
	}
	for _, tbl := range wantTables {
		if tableExists(t, s, tbl) {
			t.Fatalf("after Down: table %q still present", tbl)
		}
	}
	// Down on an empty DB is also idempotent.
	if err := Down(ctx, s.DB()); err != nil {
		t.Fatalf("Down #1b (idempotent): %v", err)
	}

	// Up again recreates everything (recoverable from a torn-down schema).
	if err := Up(ctx, s.DB()); err != nil {
		t.Fatalf("Up #2: %v", err)
	}
	v, err = Version(ctx, s.DB())
	if err != nil {
		t.Fatalf("Version after Up #2: %v", err)
	}
	if v != 2 {
		t.Fatalf("version after Up #2 = %d, want 2", v)
	}
	for _, tbl := range wantTables {
		if !tableExists(t, s, tbl) {
			t.Fatalf("after Up #2: table %q missing", tbl)
		}
	}
}

// tableExists reports whether a table named tbl exists.
func tableExists(t *testing.T, s *Store, tbl string) bool {
	t.Helper()
	var n int
	err := s.DB().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, tbl,
	).Scan(&n)
	if err != nil {
		t.Fatalf("tableExists %q: %v", tbl, err)
	}
	return n > 0
}
