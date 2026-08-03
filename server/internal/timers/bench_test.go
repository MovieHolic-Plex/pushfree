// BenchmarkClaim measures the atomic claim path against 1000 pending timers
// and reports p99 latency, which the todo-22 acceptance requires to be
// <50ms for 1000 pending claims. The bench creates 1000 due timers once,
// then claims them one-at-a-time (batch=1) to produce a latency distribution
// whose p99 (the 990th of 1000 samples) is reported via b.ReportMetric and a
// clear PASS/FAIL line.
//
// Run with:  go test ./internal/timers/... -bench BenchmarkClaim -benchtime 1x
package timers

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pushfree/pushfree/internal/store"
)

// newRand returns a deterministic *rand.Rand seeded with seed, so jitter and
// any randomized test input are reproducible (no timing luck).
func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

func BenchmarkClaim(b *testing.B) {
	s := newSQLStoreB(b)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	const n = 1000
	for i := 0; i < n; i++ {
		if _, err := s.Create(ctx, &store.Timer{Kind: KindCallback, FireAt: base}); err != nil {
			b.Fatalf("seed timer %d: %v", i, err)
		}
	}

	b.ResetTimer()
	// -benchtime 1x -> b.N == 1: one full pass of 1000 single-timer claims.
	for iter := 0; iter < b.N; iter++ {
		latencies := make([]time.Duration, 0, n)
		for claimed := 0; claimed < n; claimed++ {
			start := time.Now()
			got, err := s.ClaimDue(ctx, base, 1)
			dur := time.Since(start)
			if err != nil {
				b.Fatalf("claim %d: %v", claimed, err)
			}
			if len(got) != 1 {
				b.Fatalf("claim %d returned %d timers, want 1", claimed, len(got))
			}
			latencies = append(latencies, dur)
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		// p99 = 99th percentile of the 1000 samples = index 990 (ceil(0.99*1000)-1).
		p99 := latencies[(99*n)/100-1]
		b.ReportMetric(float64(p99.Microseconds())/1000.0, "p99_ms")
		// Verdict line so the raw bench output states p99<50ms explicitly
		// (the acceptance criterion, readable in evidence).
		verdict := "PASS"
		if p99 >= 50*time.Millisecond {
			verdict = "FAIL"
		}
		fmt.Printf("BenchmarkClaim: 1000 pending claims p99=%v (%s: p99<50ms)\n", p99, verdict)
		if verdict == "FAIL" {
			b.Fatalf("p99=%v >= 50ms", p99)
		}
	}
}

// newSQLStoreB is the benchmark variant of newSQLStore (uses b, not *testing.T).
func newSQLStoreB(b *testing.B) *sqlStore {
	b.Helper()
	dir := b.TempDir()
	path := dir + "/timers-bench.db"
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)")
	if err != nil {
		b.Fatalf("open sqlite: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(timersSchema); err != nil {
		b.Fatalf("create schema: %v", err)
	}
	return &sqlStore{db: db}
}
