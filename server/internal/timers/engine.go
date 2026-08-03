// Package timers implements the durable, crash-recoverable timer engine that
// drives all deferred server work: priority-2 retry/expire (todo 21 wiring),
// callback retries (todo 25), and quiet-hours flushes (todo 14).
//
// Design (EB/skeptic-round2 "durable timer + crash-recovery"):
//
//   - Persistence: every timer is a row in the timers table (fire_at, kind,
//     payload, claimed_at). The engine holds ZERO in-memory state about
//     pending work, so a kill -9 at any instant loses nothing that the
//     startup scan cannot rebuild from the table.
//   - Atomic claim: ClaimDue is a single UPDATE ... WHERE claimed_at IS NULL
//     AND fire_at <= ? ... RETURNING under the SQLite write lock. Two workers
//     polling the same database can never both receive the same timer.
//   - Fire-then-delete: after a timer's handler returns nil the row is
//     deleted. A surviving row therefore always means pending work, never
//     completed work.
//   - Crash recovery: ResetOrphanedClaims (one UPDATE) clears claimed_at on
//     every claimed-but-not-deleted row at startup. Those rows re-enter the
//     due-set and fire exactly once. The engine exposes Claim and Fire as
//     SEPARATE steps so a test can deterministically model "killed between
//     claim and fire" without goroutine timing.
//   - Injected clock: every time-comparison goes through Clock so the full
//     schedule runs in microseconds; the only real sleep in production is the
//     ~1s poll tick (Run's loop), which tests never enter.
//
// Layering: this package imports only internal/store (the Timer row type) and
// the pure receipts rules. The concrete SQLite Store lives in
// internal/store/sqlite/timer_engine.go and satisfies timers.Store
// structurally.
package timers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// Clock returns the current instant. Production passes time.Now; tests inject
// a controllable clock so fire_at comparisons are deterministic with no
// sleeping. Mirrors receipts.Clock.
type Clock func() time.Time

// Store is the full timer persistence surface the engine needs. It is a
// superset of store.TimerRepo (Create + ClaimDue) adding the two operations
// the engine owns: per-row Delete (fire-then-delete) and ResetOrphanedClaims
// (startup recovery). The concrete *sqlite.TimerRepo satisfies this
// structurally via internal/store/sqlite/timer_engine.go.
type Store interface {
	Create(ctx context.Context, t *store.Timer) (int64, error)
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]store.Timer, error)
	Delete(ctx context.Context, id int64) error
	ResetOrphanedClaims(ctx context.Context) (int, error)
}

// Handler executes one fired timer's deferred work and returns nil on
// success. On error the timer is NOT deleted: it stays claimed and becomes an
// orphan that the next startup scan reclaims, so failed handlers retry across
// restarts (at-least-once). The handler MUST be idempotent if it has external
// side effects, since a crash between a successful handler return and Delete
// re-fires the timer once on recovery.
type Handler func(ctx context.Context, t store.Timer) error

// Engine polls the timers table, atomically claims due rows, fires their
// handlers, and deletes them. It is safe for concurrent use: the only shared
// mutable state is handlers (set up before Run) and the Store, whose claim is
// atomic.
type Engine struct {
	store    Store
	clock    Clock
	batch    int
	handlers map[string]Handler
	// onClaimed fires AFTER a successful ClaimDue and BEFORE any handler, on
	// the claimed batch. It is the single deterministic kill point a test
	// uses to model a crash between claim and fire. Production leaves it nil.
	onClaimed func(claimed []store.Timer)
}

// Option configures an Engine.
type Option func(*Engine)

// WithBatch sets the maximum timers claimed per poll (default 100). Smaller
// batches make crash-recovery tests deterministic (claim N, kill, recover the
// rest).
func WithBatch(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.batch = n
		}
	}
}

// WithClock injects the clock used for due-comparison (default time.Now).
func WithClock(c Clock) Option {
	return func(e *Engine) {
		if c != nil {
			e.clock = c
		}
	}
}

// WithOnClaimed installs the post-claim, pre-fire hook (the deterministic kill
// point). It is intended for tests.
func WithOnClaimed(fn func(claimed []store.Timer)) Option {
	return func(e *Engine) { e.onClaimed = fn }
}

// DefaultBatch is the claim batch size when WithBatch is not used.
const DefaultBatch = 100

// NewEngine builds an Engine over s with the given options. Handlers are
// registered after construction via RegisterHandler.
func NewEngine(s Store, opts ...Option) *Engine {
	e := &Engine{
		store:    s,
		clock:    Clock(time.Now),
		batch:    DefaultBatch,
		handlers: make(map[string]Handler),
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Timer kind constants, matching the timers.kind CHECK constraint in
// 0001_init.up.sql. They are the exact strings stored in the table.
const (
	KindRetry      = "retry"
	KindExpire     = "expire"
	KindCallback   = "callback"
	KindQuietHours = "quiethours"
)

// RegisterHandler binds h to the given timer kind. The kind string must match
// a timers.kind value ("retry"|"expire"|"callback"|"quiethours"). Registering
// a kind twice replaces the handler.
func (e *Engine) RegisterHandler(kind string, h Handler) {
	e.handlers[kind] = h
}

// CreateTimer persists a new timer and returns its id. It is the scheduling
// entry point handlers use to enqueue follow-up work (e.g. the retry handler
// scheduling the next priority-2 attempt) without depending on the concrete
// Store type.
func (e *Engine) CreateTimer(ctx context.Context, t *store.Timer) (int64, error) {
	return e.store.Create(ctx, t)
}

// Recover is the startup scan. It resets every orphaned claim (claimed but
// never deleted) so the due-poll reclaims and fires those timers exactly
// once. It must run ONCE before the first Poll/Run after process start.
func (e *Engine) Recover(ctx context.Context) (int, error) {
	return e.store.ResetOrphanedClaims(ctx)
}

// Claim atomically claims up to batch due, unclaimed timers (fire_at <= now,
// claimed_at IS NULL), marking claimed_at = now and returning the claimed
// rows. Each row is handed to exactly one caller. Exposed separately from
// Fire so a test can deterministically model a kill -9 between claim and fire.
func (e *Engine) Claim(ctx context.Context) ([]store.Timer, error) {
	return e.store.ClaimDue(ctx, e.clock(), e.batch)
}

// ErrNoHandler is returned by Fire when a claimed timer's kind has no
// registered handler. The timer is still deleted (a timer with no handler is
// dead work that would otherwise block recovery forever); the error lets a
// caller observe the misconfiguration.
var ErrNoHandler = errors.New("timers: no handler registered for kind")

// Fire invokes each claimed timer's handler and, on success, deletes the row.
// A handler error leaves the row claimed (not deleted): it becomes an orphan
// reclaimed by the next startup Recover, retrying the work. ctx is checked
// before each fire so a cancelled Run stops promptly between timers.
func (e *Engine) Fire(ctx context.Context, claimed []store.Timer) (fired int, err error) {
	for _, t := range claimed {
		if err := ctx.Err(); err != nil {
			return fired, err
		}
		h, ok := e.handlers[t.Kind]
		if !ok {
			// No handler = dead work. Delete so recovery is not blocked; report it.
			_ = e.store.Delete(ctx, t.ID)
			return fired, fmt.Errorf("%w: %s", ErrNoHandler, t.Kind)
		}
		if err := h(ctx, t); err != nil {
			return fired, fmt.Errorf("fire timer %d (%s): %w", t.ID, t.Kind, err)
		}
		if err := e.store.Delete(ctx, t.ID); err != nil {
			return fired, fmt.Errorf("delete fired timer %d: %w", t.ID, err)
		}
		fired++
	}
	return fired, nil
}

// Poll is one full engine iteration: ClaimDue then Fire. It is the unit Run
// loops over and the unit a test drives when it does not need to split
// claim/fire. Between Claim and Fire the onClaimed hook (if set) runs -- the
// deterministic kill point. Returns the number of timers fired this poll.
func (e *Engine) Poll(ctx context.Context) (int, error) {
	claimed, err := e.Claim(ctx)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	if len(claimed) == 0 {
		return 0, nil
	}
	if e.onClaimed != nil {
		e.onClaimed(claimed)
	}
	if err := ctx.Err(); err != nil {
		// Killed between claim and fire (the modeled crash point): leave the
		// rows claimed so they become orphans Recover will pick up.
		return 0, err
	}
	return e.Fire(ctx, claimed)
}

// Run is the production loop: Recover once at startup, then Poll every
// interval until ctx is cancelled. The interval sleep is the ONLY real sleep
// in the engine; tests drive Poll/Claim/Fire directly and never call Run, so
// the gates (no sleeps beyond the poll tick, injected clock only) hold.
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	if _, err := e.Recover(ctx); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		// Poll immediately on entry and after each tick so Recover-primed work
		// fires without waiting a full interval.
		if _, err := e.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
