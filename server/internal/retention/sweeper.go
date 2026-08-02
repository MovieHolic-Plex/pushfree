// Package retention runs the periodic data-retention sweeper (message expiry,
// undownloaded-attachment BLOB expiry, and TTL discard) and provides the
// graceful-shutdown tail (WAL checkpoint). It depends only on the narrow
// Store interface implemented by internal/store/sqlite.
package retention

import (
	"context"
	"log/slog"
	"time"
)

// Clock returns the current time. Sweeper tests inject a fixed clock so no
// real-time sleeping is needed to exercise the age-based sweeps.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock; it wraps time.Now.
type SystemClock struct{}

// Now reports the wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// Store is the retention sweep/checkpoint surface a concrete database
// implements (internal/store/sqlite.*Store satisfies it).
type Store interface {
	DeleteMessagesBefore(ctx context.Context, cutoff time.Time) (int64, error)
	ClearUndownloadedAttachmentBLOBs(ctx context.Context, cutoff time.Time) (int64, error)
	DeleteUndeliveredExpiredByTTL(ctx context.Context, now time.Time) (int64, error)
}

// Stats reports what a single sweep pass accomplished. Surfaced via Run's
// per-cycle log line and returned from SweepOnce for tests.
type Stats struct {
	MessagesDeleted        int64
	AttachmentBlobsCleared int64
	TTLDiscarded           int64
}

// Sweeper runs the retention sweeps on a fixed interval using an injected
// clock for all age calculations. It is safe to Run in a single goroutine;
// Run returns when ctx is canceled (graceful shutdown).
type Sweeper struct {
	store        Store
	clock        Clock
	interval     time.Duration
	msgRetention time.Duration
	attRetention time.Duration
	logger       *slog.Logger
}

// NewSweeper builds a Sweeper from duration strings (the config form), so a
// malformed interval/retention fails loudly at startup rather than at first
// sweep. ttl/retention of zero disables the corresponding pass (a zero
// messages-retention keeps messages forever; a zero attachment-retention
// never clears BLOBs).
func NewSweeper(st Store, clock Clock, interval, msgRetention, attRetention string, logger *slog.Logger) (*Sweeper, error) {
	if clock == nil {
		clock = SystemClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	parsed, err := parseDurations(map[string]string{
		"sweeper-interval":     interval,
		"messages-retention":   msgRetention,
		"attachment-retention": attRetention,
	})
	if err != nil {
		return nil, err
	}
	if parsed["sweeper-interval"] <= 0 {
		return nil, errNonPositive("sweeper-interval", interval)
	}
	return &Sweeper{
		store:        st,
		clock:        clock,
		interval:     parsed["sweeper-interval"],
		msgRetention: parsed["messages-retention"],
		attRetention: parsed["attachment-retention"],
		logger:       logger,
	}, nil
}

// SweepOnce runs all three retention passes once at the clock's current time
// and returns the aggregate stats. It honors ctx so it cancels promptly when
// the server is shutting down. A zero retention duration skips that pass.
func (sw *Sweeper) SweepOnce(ctx context.Context) (Stats, error) {
	now := sw.clock.Now()
	var st Stats

	if sw.msgRetention > 0 {
		cutoff := now.Add(-sw.msgRetention)
		n, err := sw.store.DeleteMessagesBefore(ctx, cutoff)
		if err != nil {
			return st, err
		}
		st.MessagesDeleted = n
	}

	if sw.attRetention > 0 {
		cutoff := now.Add(-sw.attRetention)
		n, err := sw.store.ClearUndownloadedAttachmentBLOBs(ctx, cutoff)
		if err != nil {
			return st, err
		}
		st.AttachmentBlobsCleared = n
	}

	// TTL discard is independent of the retention windows: any send whose
	// own ttl has elapsed drops its undelivered messages.
	n, err := sw.store.DeleteUndeliveredExpiredByTTL(ctx, now)
	if err != nil {
		return st, err
	}
	st.TTLDiscarded = n

	return st, nil
}

// Run sweeps once immediately (so a freshly-restarted server reclaims stale
// data right away) and then every interval until ctx is canceled. Each cycle
// logs its stats. It always returns nil; per-cycle errors are logged and do
// not stop the loop (a transient failure should not kill retention).
func (sw *Sweeper) Run(ctx context.Context) error {
	// First sweep immediately on startup.
	sw.runCycle(ctx)

	t := time.NewTicker(sw.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			sw.runCycle(ctx)
		}
	}
}

func (sw *Sweeper) runCycle(ctx context.Context) {
	st, err := sw.SweepOnce(ctx)
	if err != nil {
		// context.Canceled is expected during graceful shutdown: the
		// in-flight sweep is aborted by the shutdown context. Don't log
		// it as an error.
		if ctx.Err() != nil {
			return
		}
		sw.logger.Error("retention sweep failed", "err", err)
		return
	}
	if st.MessagesDeleted == 0 && st.AttachmentBlobsCleared == 0 && st.TTLDiscarded == 0 {
		return // nothing to log on a clean no-op cycle
	}
	sw.logger.Info("retention sweep complete",
		"messages_deleted", st.MessagesDeleted,
		"attachment_blobs_cleared", st.AttachmentBlobsCleared,
		"ttl_discarded", st.TTLDiscarded,
	)
}

// Interval exposes the configured sweep interval (used by tests/operators).
func (sw *Sweeper) Interval() time.Duration { return sw.interval }
