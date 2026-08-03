package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)

	"github.com/pushfree/pushfree/internal/store"
)

// driverName is the modernc.org/sqlite registered driver name (pure Go).
const driverName = "sqlite"

// queryExec is the subset of *sql.DB/*sql.Tx the repos use. Accepting either
// lets every repo run inside the fan-out transaction or against the pool.
type queryExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB is what the migration runner needs: query/exec plus the ability to begin
// a transaction. *sql.DB satisfies it.
type DB interface {
	queryExec
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// ctxExec / ctxExecBegin are the minimal slices of DB used by migrate.go,
// kept as aliases for readability.
type ctxExec = queryExec

type ctxExecBegin interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// Pragmas applied on every connection via the modernc _pragma DSN param.
//
//   - journal_mode=WAL: required append-only write log for concurrency.
//   - busy_timeout=5000: serialize concurrent writers without SQLITE_BUSY.
//   - foreign_keys=1: enforce REFERENCES so the 1:1 and FK guarantees are
//     real, not advisory. This is an addition over the two pragmas named in
//     the plan; without it SQLite silently allows orphaned foreign keys.
func buildDSN(dbPath string) string {
	// url.PathEscape keeps Windows backslashes/spaces safe in the query,
	// but the path itself is passed verbatim before the '?' separator.
	pragmas := url.Values{}
	pragmas.Set("_pragma", "foreign_keys(1)")
	pragmas.Set("_pragma", "journal_mode(WAL)")
	pragmas.Set("_pragma", "busy_timeout(5000)")
	return dbPath + "?" + pragmas.Encode()
}

// OpenDB opens the SQLite database at dbPath, applies pragmas, and runs all
// pending up migrations. It returns the raw *sql.DB so callers can build
// ad-hoc repositories. Most callers should use Open.
func OpenDB(ctx context.Context, dbPath string) (*sql.DB, error) {
	db, err := sql.Open(driverName, buildDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", dbPath, err)
	}
	if err := Up(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// OpenDBRaw opens the database with pragmas but WITHOUT running migrations,
// so tests can exercise the migration runner explicitly.
func OpenDBRaw(ctx context.Context, dbPath string) (*sql.DB, error) {
	db, err := sql.Open(driverName, buildDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", dbPath, err)
	}
	return db, nil
}

// Store is the concrete SQLite implementation of store.Repos.
type Store struct {
	db    *sql.DB
	users *UserRepo
	apps  *AppRepo
	devs  *DeviceRepo
	sends *SendRepo
	msgs  *MessageRepo
	atts  *AttachmentRepo
	rcpts *ReceiptRepo
	quota *QuotaRepo
	timrs *TimerRepo
	cbs   *CallbackRepo
	ing   *IngestRepo
	grps  *GroupRepo
	subs  *SubscriptionRepo
}

// Open opens (and migrates) the database and returns a fully wired Store.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	db, err := OpenDB(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	return NewStore(db), nil
}

// NewStore wires a Store around an already-open *sql.DB. The caller owns the
// connection's lifecycle (including Close).
func NewStore(db *sql.DB) *Store {
	return &Store{
		db:    db,
		users: &UserRepo{db: db},
		apps:  &AppRepo{db: db},
		devs:  &DeviceRepo{db: db},
		sends: &SendRepo{db: db},
		msgs:  &MessageRepo{db: db},
		atts:  &AttachmentRepo{db: db},
		rcpts: &ReceiptRepo{db: db},
		quota: &QuotaRepo{db: db},
		timrs: &TimerRepo{db: db},
		cbs:   &CallbackRepo{db: db},
		ing:   &IngestRepo{db: db},
		grps:  &GroupRepo{db: db},
		subs:  &SubscriptionRepo{db: db},
	}
}

// DB exposes the underlying connection pool for advanced callers.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// Repos returns the store.Repos bundle of interfaces.
func (s *Store) Repos() store.Repos {
	return store.Repos{
		Users:         s.users,
		Apps:          s.apps,
		Devices:       s.devs,
		Sends:         s.sends,
		Messages:      s.msgs,
		Attachments:   s.atts,
		Receipts:      s.rcpts,
		Quota:         s.quota,
		Timers:        s.timrs,
		Callbacks:     s.cbs,
		Ingests:       s.ing,
		Groups:        s.grps,
		Subscriptions: s.subs,
	}
}

// mapErr translates a database/sql error into a store sentinel where
// possible. modernc.org/sqlite returns errors whose SQLite extended result
// code is 2067 (SQLITE_CONSTRAINT_UNIQUE) for unique violations; the message
// text also contains "UNIQUE constraint failed", so we match both to be
// robust across driver versions.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	msg := err.Error()
	// modernc: "constraint failed: UNIQUE constraint failed: users.user_key (2067)"
	if containsAny(msg, "UNIQUE constraint failed", "constraint failed: UNIQUE", "PRIMARY KEY constraint failed") {
		return fmt.Errorf("%w: %s", store.ErrUniqueViolation, msg)
	}
	return err
}

// containsAny reports whether s contains any of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub == "" {
			continue
		}
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
