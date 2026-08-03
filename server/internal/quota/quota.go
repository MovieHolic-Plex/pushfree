// Package quota enforces the per-user monthly send limit.
//
// The billing period is the calendar month ("YYYY-MM") in America/Chicago
// (Central Time), resetting at the first instant (00:00:00) of each month in
// that zone. DST is resolved by the injected *time.Location, so the reset
// epoch is 05:00:00 UTC during CDT (summer, -05:00) and 06:00:00 UTC during
// CST (winter, -06:00). The limit is shared across ALL of a user's apps, and
// only successful SENDS are charged -- delivery retries (receipts) never
// re-count; that regression is owned by todo 26.
//
// The clock and reset zone are injectable so the month-boundary behavior is
// deterministic in tests. Production uses wall time and CentralTime.
package quota

import (
	"context"
	"errors"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// DefaultLimit is the Pushover per-user monthly send quota (plan todo 4
// default quota-monthly). It is the fallback used by NewManager when a
// non-positive limit is passed.
const DefaultLimit = 10000

// CentralTZ is the IANA zone whose calendar-month boundaries govern the
// quota reset (Pushover resets at 00:00 U.S. Central Time on the 1st).
const CentralTZ = "America/Chicago"

// CentralTime is the parsed America/Chicago location. It is the default
// reset zone for every Manager. If the host has no zoneinfo (some minimal
// Windows/container images), it falls back to a fixed -06:00 zone; that
// fallback is off by one hour during CDT but only matters within an hour of
// a month boundary, and real deployments ship zoneinfo (the quiethours
// package depends on the same LoadLocation).
var CentralTime = mustLoad(CentralTZ)

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone("CST", -6*3600)
	}
	return loc
}

// Clock abstracts wall time so month-boundary reset is testable with a
// fixed or stepped clock. SystemClock returns the real time.Now.
type Clock interface {
	Now() time.Time
}

// SystemClock is the default Clock: it reports wall time.
type SystemClock struct{}

// Now returns the current wall time.
func (SystemClock) Now() time.Time { return time.Now() }

// Period returns the "YYYY-MM" billing period containing now, evaluated in
// loc. Two instants share a period iff they fall in the same calendar month
// of loc (DST-safe: the month is computed in loc, not in UTC).
func Period(loc *time.Location, now time.Time) string {
	return now.In(loc).Format("2006-01")
}

// MonthStart returns the first instant (00:00:00) of the month containing
// now, in loc. It is the inclusive lower bound of the current period and the
// building block for NextReset.
func MonthStart(loc *time.Location, now time.Time) time.Time {
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
}

// NextReset returns the epoch-second timestamp at which the current period
// ends: the first instant (00:00:00) of the following month in loc. This is
// the value surfaced in X-Limit-App-Reset and limits.json "reset".
func NextReset(loc *time.Location, now time.Time) int64 {
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc)
	return next.Unix()
}

// ErrOverLimit is the sentinel for a charge that would exceed the monthly
// limit. It is returned by Reserve when the conditional increment refused.
var ErrOverLimit = errors.New("quota: monthly limit exceeded")

// Manager gates per-user monthly sends against a backing QuotaRepo. The
// limit, reset zone, and clock are all injectable; it is safe for concurrent
// use only insofar as the backing repo is (the SQLite repo serializes writers
// via a single write lock).
type Manager struct {
	repo  store.QuotaRepo
	limit int64
	loc   *time.Location
	clock Clock
}

// NewManager builds a quota Manager. A nil loc defaults to CentralTime; a nil
// clock defaults to SystemClock; limit <= 0 falls back to DefaultLimit. The
// repo MUST be non-nil.
func NewManager(repo store.QuotaRepo, limit int64, loc *time.Location, clock Clock) *Manager {
	if loc == nil {
		loc = CentralTime
	}
	if clock == nil {
		clock = SystemClock{}
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &Manager{repo: repo, limit: limit, loc: loc, clock: clock}
}

// Limit returns the configured monthly limit.
func (m *Manager) Limit() int64 { return m.limit }

// Now returns the manager's current time (wall time unless a clock was
// injected).
func (m *Manager) Now() time.Time { return m.clock.Now() }

// PeriodNow returns the current billing period.
func (m *Manager) PeriodNow() string { return Period(m.loc, m.clock.Now()) }

// ResetNow returns the epoch-second reset for the current period.
func (m *Manager) ResetNow() int64 { return NextReset(m.loc, m.clock.Now()) }

// Snapshot returns (count, limit, remaining, resetEpoch) for userID in the
// current period. A never-touched period yields count=0 (the repo returns a
// zero QuotaCounter, not an error). remaining is clamped at 0. A real store
// error is returned so callers can fail closed/open deliberately.
func (m *Manager) Snapshot(ctx context.Context, userID int64) (count, limit, remaining, reset int64, err error) {
	reset = m.ResetNow()
	limit = m.limit
	qc, qerr := m.repo.Get(ctx, userID, m.PeriodNow())
	if qerr != nil {
		return 0, limit, 0, reset, qerr
	}
	count = qc.Count
	remaining = limit - count
	if remaining < 0 {
		remaining = 0
	}
	return count, limit, remaining, reset, nil
}

// Allow reports whether userID may send n more messages this period WITHOUT
// mutating the counter, plus the post-charge remaining and the reset epoch.
// It is the pre-write gate (called before persisting a send). n <= 0 is
// always allowed and charges nothing. A store read error yields
// allowed=false so a transient outage cannot leak quota (fail closed).
func (m *Manager) Allow(ctx context.Context, userID, n int64) (allowed bool, remainingAfter int64, reset int64, err error) {
	_, _, remaining, reset, err := m.Snapshot(ctx, userID)
	if err != nil {
		return false, 0, reset, err
	}
	if n <= 0 {
		return true, remaining, reset, nil
	}
	if n > remaining {
		return false, 0, reset, nil
	}
	return true, remaining - n, reset, nil
}

// Charge atomically adds n to the counter and returns the post-charge
// remaining (clamped at 0). It does NOT enforce the limit; pair it with Allow
// as the pre-write gate so the post-write Charge reflects exactly the sends
// that passed the gate. n <= 0 is a no-op returning the current remaining.
func (m *Manager) Charge(ctx context.Context, userID, n int64) (remaining int64, err error) {
	if n <= 0 {
		_, _, remaining, _, err := m.Snapshot(ctx, userID)
		return remaining, err
	}
	after, err := m.repo.Increment(ctx, userID, m.PeriodNow(), n)
	if err != nil {
		return 0, err
	}
	remaining = m.limit - after
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}
