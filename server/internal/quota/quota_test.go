package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// fakeRepo is an in-memory store.QuotaRepo for deterministic quota tests. It
// serializes Increment with a mutex so the post-increment read is consistent
// (mirroring the SQLite repo's in-transaction read-back).
type fakeRepo struct {
	mu       sync.Mutex
	counters map[string]int64 // key: userID|period
	fail     error            // when set, Get/Increment return this
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{counters: make(map[string]int64)}
}

func key(userID int64, period string) string {
	return strconvFormatInt(userID) + "|" + period
}

func strconvFormatInt(n int64) string {
	// avoid pulling strconv just for this; manual int64 -> string
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (f *fakeRepo) Increment(_ context.Context, userID int64, period string, delta int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	k := key(userID, period)
	f.counters[k] += delta
	return f.counters[k], nil
}

func (f *fakeRepo) Get(_ context.Context, userID int64, period string) (store.QuotaCounter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return store.QuotaCounter{}, f.fail
	}
	k := key(userID, period)
	return store.QuotaCounter{UserID: userID, Period: period, Count: f.counters[k]}, nil
}

// fixedClock returns a fixed time, so Period/NextReset/Snapshot are
// deterministic at the month boundary.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// TestPeriodCentralTimeBoundary covers the acceptance criterion "injected-
// clock month boundary (last day 23:59 CT -> next month 00:00 CT)". One minute
// either side of the 2026-07 -> 2026-08 boundary must land in distinct
// periods, and the reset must be 2026-08-01 00:00:00 CDT (== 05:00:00 UTC).
func TestPeriodCentralTimeBoundary(t *testing.T) {
	loc := CentralTime
	const uid int64 = 1

	// 2026-07-31 23:59:00 CDT -- last instant-but-one of July in CT.
	lastOfJuly := time.Date(2026, 7, 31, 23, 59, 0, 0, loc)
	// 2026-08-01 00:00:00 CDT -- first instant of August in CT.
	firstOfAug := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)

	if got := Period(loc, lastOfJuly); got != "2026-07" {
		t.Fatalf("Period(23:59 Jul 31 CT)=%q want 2026-07", got)
	}
	if got := Period(loc, firstOfAug); got != "2026-08" {
		t.Fatalf("Period(00:00 Aug 01 CT)=%q want 2026-08", got)
	}

	// Reset at 23:59 Jul 31 must point at the Aug 01 boundary.
	rst := NextReset(loc, lastOfJuly)
	gotReset := time.Unix(rst, 0).In(loc)
	wantReset := firstOfAug
	if !gotReset.Equal(wantReset) {
		t.Fatalf("NextReset(23:59 Jul 31 CT)=%v want %v", gotReset, wantReset)
	}
	// In UTC that boundary is 05:00:00 (CDT == UTC-5 during summer).
	if u := time.Unix(rst, 0).UTC(); u.Hour() != 5 {
		t.Fatalf("Aug reset in UTC=%v, want hour 5 (CDT)", u)
	}

	// Manager wired to the pre-boundary clock: a July charge must NOT count
	// once the clock steps into August (period rolls over, counter starts at 0).
	m := NewManager(newFakeRepo(), DefaultLimit, loc, fixedClock{lastOfJuly})
	if _, err := m.Charge(context.Background(), uid, 5); err != nil {
		t.Fatalf("charge in July: %v", err)
	}
	if c, _, rem, _, _ := m.Snapshot(context.Background(), uid); c != 5 || rem != DefaultLimit-5 {
		t.Fatalf("July snapshot: count=%d remaining=%d want 5/%d", c, rem, DefaultLimit-5)
	}
	// Step the clock to August: same Manager, new period -> counter is 0.
	m2 := NewManager(m.repo.(*fakeRepo), DefaultLimit, loc, fixedClock{firstOfAug})
	if c, _, rem, rst2, _ := m2.Snapshot(context.Background(), uid); c != 0 || rem != DefaultLimit {
		t.Fatalf("August snapshot: count=%d remaining=%d want 0/%d (no carry-over)", c, rem, DefaultLimit)
	} else if time.Unix(rst2, 0).In(loc).Month() != time.September {
		t.Fatalf("August reset month=%v want September", time.Unix(rst2, 0).In(loc).Month())
	}
}

// TestWinterBoundaryDST verifies the winter reset is 06:00 UTC (CST, -06:00),
// distinct from the summer 05:00 UTC. This locks DST handling.
func TestWinterBoundaryDST(t *testing.T) {
	loc := CentralTime
	jan := time.Date(2026, 1, 15, 12, 0, 0, 0, loc)
	rst := NextReset(loc, jan)
	got := time.Unix(rst, 0).UTC()
	want := time.Date(2026, 2, 1, 6, 0, 0, 0, time.UTC) // Feb 01 00:00 CST == 06:00 UTC
	if !got.Equal(want) {
		t.Fatalf("winter NextReset UTC=%v want %v (CST)", got, want)
	}
}

// TestAllowLimitBoundary covers "10000 ok / 10001 -> rejected (429)". Charging
// to exactly the limit leaves remaining 0, so one more send is refused.
func TestAllowLimitBoundary(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, CentralTime)
	m := NewManager(newFakeRepo(), DefaultLimit, CentralTime, fixedClock{now})
	ctx := context.Background()
	const uid int64 = 7

	// 9999 charged: one more (n=1) fits.
	if _, err := m.Charge(ctx, uid, DefaultLimit-1); err != nil {
		t.Fatalf("charge to limit-1: %v", err)
	}
	allowed, remAfter, _, err := m.Allow(ctx, uid, 1)
	if err != nil || !allowed {
		t.Fatalf("Allow(1) at count=%d: allowed=%v err=%v, want true", DefaultLimit-1, allowed, err)
	}
	if remAfter != 0 {
		t.Fatalf("remaining-after=%d want 0 (exactly at limit)", remAfter)
	}

	// Charge the last unit -> count == limit. The next single send is refused.
	if _, err := m.Charge(ctx, uid, 1); err != nil {
		t.Fatalf("charge final unit: %v", err)
	}
	allowed, _, rst, err := m.Allow(ctx, uid, 1)
	if err != nil {
		t.Fatalf("Allow at limit: err=%v", err)
	}
	if allowed {
		t.Fatalf("Allow(1) at count=%d: allowed=true, want false (the 10001st)", DefaultLimit)
	}
	// Reset epoch must still be populated on refusal (X-Limit-App-Reset).
	if rst == 0 {
		t.Fatalf("reset epoch not returned on refusal")
	}
}

// TestAllowGroupPartialOverflow: a single send to N recipients is rejected as
// a whole when N exceeds remaining -- no partial acceptance.
func TestAllowGroupPartialOverflow(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, CentralTime)
	m := NewManager(newFakeRepo(), DefaultLimit, CentralTime, fixedClock{now})
	ctx := context.Background()
	const uid int64 = 9

	// remaining == 1 after charging limit-1.
	if _, err := m.Charge(ctx, uid, DefaultLimit-1); err != nil {
		t.Fatalf("charge: %v", err)
	}
	// A 3-recipient group send needs 3 but only 1 remains -> refused.
	allowed, _, _, err := m.Allow(ctx, uid, 3)
	if err != nil || allowed {
		t.Fatalf("Allow(3) with remaining 1: allowed=%v err=%v, want false", allowed, err)
	}
	// A 1-recipient send still fits.
	allowed, _, _, err = m.Allow(ctx, uid, 1)
	if err != nil || !allowed {
		t.Fatalf("Allow(1) with remaining 1: allowed=%v err=%v, want true", allowed, err)
	}
}

// TestSnapshotConsistency locks the limits.json field set: count+remaining ==
// limit, reset is the next CT month boundary, and a fresh user sees the full
// quota.
func TestSnapshotConsistency(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, CentralTime)
	m := NewManager(newFakeRepo(), DefaultLimit, CentralTime, fixedClock{now})
	ctx := context.Background()
	const uid int64 = 11

	count, limit, remaining, reset, err := m.Snapshot(ctx, uid)
	if err != nil {
		t.Fatalf("snapshot fresh: %v", err)
	}
	if count != 0 || limit != DefaultLimit || remaining != DefaultLimit {
		t.Fatalf("fresh snapshot: count=%d limit=%d remaining=%d want 0/%d/%d", count, limit, remaining, DefaultLimit, DefaultLimit)
	}
	if count+remaining != limit {
		t.Fatalf("inconsistent: count(%d)+remaining(%d) != limit(%d)", count, remaining, limit)
	}
	rt := time.Unix(reset, 0).In(CentralTime)
	if rt.Day() != 1 || rt.Hour() != 0 || rt.Minute() != 0 || rt.Second() != 0 || rt.Month() != time.August {
		t.Fatalf("reset %v is not 2026-08-01 00:00 CT", rt)
	}

	// After charging K, count==K and remaining==limit-K.
	const k = 4242
	if _, err := m.Charge(ctx, uid, k); err != nil {
		t.Fatalf("charge: %v", err)
	}
	count, limit, remaining, _, err = m.Snapshot(ctx, uid)
	if err != nil || count != k || remaining != DefaultLimit-k || count+remaining != limit {
		t.Fatalf("after charge: count=%d remaining=%d err=%v want %d/%d", count, remaining, err, k, DefaultLimit-k)
	}
}

// TestChargeClampsRemainingAtZero drives the counter past the limit and
// asserts remaining never goes negative.
func TestChargeClampsRemainingAtZero(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, CentralTime)
	m := NewManager(newFakeRepo(), DefaultLimit, CentralTime, fixedClock{now})
	ctx := context.Background()
	const uid int64 = 13
	rem, err := m.Charge(ctx, uid, DefaultLimit+50)
	if err != nil {
		t.Fatalf("charge over limit: %v", err)
	}
	if rem != 0 {
		t.Fatalf("remaining=%d want 0 after exceeding limit", rem)
	}
}

// TestAllowFailsClosedOnStoreError: a transient store read failure must NOT
// permit a send (fail closed so quota cannot escape).
func TestAllowFailsClosedOnStoreError(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, CentralTime)
	repo := newFakeRepo()
	boom := errors.New("disk fell over")
	repo.fail = boom
	m := NewManager(repo, DefaultLimit, CentralTime, fixedClock{now})
	allowed, _, _, err := m.Allow(context.Background(), 1, 1)
	if allowed {
		t.Fatalf("Allow with failing store: allowed=true, want false (fail closed)")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Allow err=%v want %v", err, boom)
	}
}

// TestPeriodIsComputedInZoneNotUTC guards against a UTC-regression. Central
// Time LAGS UTC, so the disagreement instant is 2026-08-01 00:00 UTC, which is
// still 2026-07-31 19:00 CDT: the CT period must be July, even though a
// UTC-based period would wrongly report August.
func TestPeriodIsComputedInZoneNotUTC(t *testing.T) {
	loc := CentralTime
	t1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) // midnight UTC
	if got := Period(loc, t1); got != "2026-07" {
		t.Fatalf("Period(2026-08-01 00:00 UTC)=%q want 2026-07 (CT view), not UTC", got)
	}
	// Sanity: the same instant expressed in UTC really is August, proving the
	// two zones disagree about the month here.
	if t1.UTC().Month() != time.August {
		t.Fatalf("sanity: UTC month=%v want August", t1.UTC().Month())
	}
	// And the reset computed from this instant must be the August boundary
	// (00:00 Aug 01 CT == 05:00 UTC), not the September one.
	rst := time.Unix(NextReset(loc, t1), 0).UTC()
	if rst.Month() != time.August || rst.Day() != 1 || rst.Hour() != 5 {
		t.Fatalf("NextReset=%v want 2026-08-01 05:00 UTC", rst)
	}
}

// TestNewManagerDefaults verifies loc/clock/limit fallbacks so production
// callers can pass nils and 0 confidently.
func TestNewManagerDefaults(t *testing.T) {
	m := NewManager(newFakeRepo(), 0, nil, nil)
	if m.Limit() != DefaultLimit {
		t.Fatalf("limit=%d want default %d", m.Limit(), DefaultLimit)
	}
	if m.loc != CentralTime {
		t.Fatalf("loc not defaulted to CentralTime")
	}
	if _, ok := m.clock.(SystemClock); !ok {
		t.Fatalf("clock not defaulted to SystemClock")
	}
}
