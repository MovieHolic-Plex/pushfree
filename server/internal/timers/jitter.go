// Jitter helper for the durable timer engine (todo 22).
//
// Per EB/skeptic-round2 and the todo-22 spec, scheduled fire times carry
// ±10% jitter so a burst of timers due at the same instant (e.g. many
// priority-2 retries scheduled off one send) do not all fire in the same
// poll. Jitter is applied when SCHEDULING (writing fire_at), never when
// claiming -- ClaimDue is exact (fire_at <= now) so jitter does not delay
// an already-due timer unpredictably.
package timers

import (
	"math/rand"
	"time"
)

// JitterFraction is the maximum fractional deviation from the nominal
// interval: 0.1 == ±10%, matching the todo-22 spec ("jitter ±10%").
const JitterFraction = 0.1

// JitterDuration returns d scaled by a factor in [1-JitterFraction,
// 1+JitterFraction], drawn from rng. Pass a deterministic *rand.Rand in tests
// (seeded) so the spread is reproducible; pass rand.New(rand.NewSource(...))
// derived from the clock in production. A nil rng returns d unchanged.
func JitterDuration(d time.Duration, rng *rand.Rand) time.Duration {
	if rng == nil || d <= 0 {
		return d
	}
	// span = 2*JitterFraction; midpoint = 1 - JitterFraction.
	span := 2 * JitterFraction
	f := (1 - JitterFraction) + rng.Float64()*span
	return time.Duration(float64(d) * f)
}

// JitterFireAt returns the instant base+d with ±JitterFraction applied to d,
// using rng. Convenience for scheduling the next retry/callback timer.
func JitterFireAt(base time.Time, d time.Duration, rng *rand.Rand) time.Time {
	return base.Add(JitterDuration(d, rng))
}
