package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// newDB opens a fresh temp-file database with migrations applied and returns
// the wired Store plus a cleanup. A temp file (not :memory:) is used so WAL
// mode and real connection-pool locking are exercised exactly as in
// production, which matters for the concurrent-claim test.
func newDB(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pushfree-test.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newRawDB opens a fresh temp-file database WITHOUT running migrations, so the
// migration runner can be exercised explicitly.
func newRawDB(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pushfree-raw.db")
	db, err := OpenDBRaw(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenDBRaw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db), path
}

// key30 returns a deterministic 30-char [0-9] string unique per seed, valid
// for user_key, token, and receipt_id CHECK(length=30).
func key30(seed int) string {
	return fmt.Sprintf("%030d", seed)
}

// mustSeedUser inserts one user and returns it with the assigned ID set.
func mustSeedUser(t *testing.T, r interface {
	Create(context.Context, *store.User) (int64, error)
}, seed int, email string) store.User {
	t.Helper()
	u := store.User{
		Email:     email,
		PassHash:  "$argon2id$...",
		Role:      "user",
		UserKey:   key30(seed),
		QuietTZ:   "UTC",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	id, err := r.Create(context.Background(), &u)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	u.ID = id
	return u
}

// mustSeedApp inserts one app for userID.
func mustSeedApp(t *testing.T, r interface {
	Create(context.Context, *store.App) (int64, error)
}, userID int64, seed int) store.App {
	t.Helper()
	a := store.App{UserID: userID, Token: key30(seed), Name: "app"}
	id, err := r.Create(context.Background(), &a)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	a.ID = id
	return a
}

