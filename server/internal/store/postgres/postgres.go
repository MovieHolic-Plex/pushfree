package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/pushfree/pushfree/internal/store"
)

// driverName is the pgx v5 stdlib registered driver name. Using pgx through
// database/sql lets this package reuse the same queryExec / inTx / scanner
// abstractions as the SQLite implementation; the dialect split lives in the
// SQL strings ($N placeholders, RETURNING, BOOLEAN, TIMESTAMPTZ, ON CONFLICT).
const driverName = "pgx"

// queryExec is the subset of *sql.DB/*sql.Tx the repos use. Accepting either
// lets every repo run inside the fan-out transaction or against the pool, the
// same pattern the SQLite backend uses.
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
// kept as aliases for readability (mirrors the SQLite package).
type ctxExec = queryExec

type ctxExecBegin interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// Open opens the Postgres database at dsn, pings it, and runs all pending up
// migrations. dsn is a pgx connection string, e.g.
// "postgres://user:pass@host:5432/dbname?sslmode=disable". It returns the
// concrete *Store whose Repos() satisfies store.Repos.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := Up(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return NewStore(db), nil
}

// OpenRaw opens the database WITHOUT running migrations, so tests can exercise
// the migration runner explicitly (and reset the schema between subtests).
func OpenRaw(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}

// Store is the concrete Postgres implementation of store.Repos.
type Store struct {
	db        *sql.DB
	users     *UserRepo
	apps      *AppRepo
	devices   *DeviceRepo
	sends     *SendRepo
	messages  *MessageRepo
	atts      *AttachmentRepo
	receipts  *ReceiptRepo
	quota     *QuotaRepo
	timers    *TimerRepo
	callbacks *CallbackRepo
	ing       *IngestRepo
	groups    *GroupRepo
	subs      *SubscriptionRepo
}

// NewStore wires a Store around an already-open *sql.DB. The caller owns the
// connection's lifecycle (including Close).
func NewStore(db *sql.DB) *Store {
	return &Store{
		db:        db,
		users:     &UserRepo{db: db},
		apps:      &AppRepo{db: db},
		devices:   &DeviceRepo{db: db},
		sends:     &SendRepo{db: db},
		messages:  &MessageRepo{db: db},
		atts:      &AttachmentRepo{db: db},
		receipts:  &ReceiptRepo{db: db},
		quota:     &QuotaRepo{db: db},
		timers:    &TimerRepo{db: db},
		callbacks: &CallbackRepo{db: db},
		ing:       &IngestRepo{db: db},
		groups:    &GroupRepo{db: db},
		subs:      &SubscriptionRepo{db: db},
	}
}

// DB exposes the underlying connection pool for advanced callers.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// Repos returns the store.Repos bundle of interfaces, identical in shape to
// the SQLite backend's bundle.
func (s *Store) Repos() store.Repos {
	return store.Repos{
		Users:         s.users,
		Apps:          s.apps,
		Devices:       s.devices,
		Sends:         s.sends,
		Messages:      s.messages,
		Attachments:   s.atts,
		Receipts:      s.receipts,
		Quota:         s.quota,
		Timers:        s.timers,
		Callbacks:     s.callbacks,
		Ingests:       s.ing,
		Groups:        s.groups,
		Subscriptions: s.subs,
	}
}

// TimerEngine returns the concrete *TimerRepo so the timers engine can be
// wired against Delete and ResetOrphanedClaims in addition to Create/ClaimDue.
func (s *Store) TimerEngine() *TimerRepo { return s.timers }

// ReceiptRepo returns the concrete *ReceiptRepo so the timer engine's retry
// handler can be wired against the receipts.RetryStore surface
// (GetReceipt/IncrementRetry/SetExpired) in addition to store.ReceiptRepo.
func (s *Store) ReceiptRepo() *ReceiptRepo { return s.receipts }

// inTx runs fn inside a database transaction. If fn returns nil the
// transaction is committed; otherwise it is rolled back and fn's error is
// returned (mapped by mapErr). The fn receives a queryExec bound to the
// transaction so all its statements share the same connection and atomicity
// boundary. Mirrors the SQLite package's inTx.
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

// scanner is the shared Scan surface of *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// mapErr translates a database/sql / pgx error into a store sentinel where
// possible. pgx returns *pgconn.PgError with stable SQLSTATE codes; 23505 is
// unique_violation. A string fallback covers any driver version that does not
// surface the typed error through database/sql.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" { // unique_violation
			return fmt.Errorf("%w: %s", store.ErrUniqueViolation, pgErr.Error())
		}
	}
	return err
}

// ----- shared nullable helpers (dialect-specific) -----

// optStr returns nil for "" so optional TEXT columns store NULL rather than
// the empty string (lets CHECK ... IS NULL and partial indexes work).
func optStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// timeArg returns a driver value for a nullable TIMESTAMPTZ column: nil for a
// nil pointer (NULL), otherwise the bare time.Time (pgx binds it directly).
func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// nullStr converts a nullable TEXT column to a Go string (NULL -> "").
func nullStr(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

// nullTime converts a nullable TIMESTAMPTZ column to a *time.Time (NULL ->
// nil).
func nullTime(v sql.NullTime) *time.Time {
	if v.Valid {
		t := v.Time
		return &t
	}
	return nil
}
