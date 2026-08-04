package server

// This file implements the priority-2 retry wiring that connects the timer
// engine, the receipt retry scheduler, and the live hub into two adapters:
//
//   - retrySeeder implements api.RetrySeeder. It creates the initial "retry"
//     timer for a priority-2 send right after ingest.
//   - redeliverFunc implements receipts.Redeliver. It re-publishes the
//     emergency send to all recipients' live WS/SSE connections on each retry.
//
// Both are constructed in main.go (or MountRealtime) and injected into the
// timer engine's retry handler and the api.Accounts group respectively.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/pushfree/pushfree/internal/receipts"
	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/timers"
)

// RetrySeeder implements api.RetrySeeder by creating the first "retry" timer
// row through the timers engine's store. It is the bootstrap that starts the
// emergency retry cycle; once the first timer fires, the retry handler in
// internal/timers/retry.go drives the rest.
type RetrySeeder struct {
	engine *timers.Engine
	policy receipts.RetryPolicy
	logger *slog.Logger
}

// NewRetrySeeder builds a RetrySeeder.
func NewRetrySeeder(engine *timers.Engine, policy receipts.RetryPolicy, logger *slog.Logger) *RetrySeeder {
	return &RetrySeeder{engine: engine, policy: policy, logger: logger}
}

// SeedRetry creates the initial retry timer. retryInterval and expireSeconds
// come from the messages.json form params (0 means "use defaults"). The timer
// fires immediately — the first attempt is due at createdAt.
func (s *RetrySeeder) SeedRetry(ctx context.Context, receiptID string, retryInterval, expireSeconds int, createdAt time.Time) error {
	policy := s.policy
	if retryInterval > 0 {
		policy.RetryInterval = time.Duration(retryInterval) * time.Second
	}
	if expireSeconds > 0 {
		policy.Expire = time.Duration(expireSeconds) * time.Second
	}
	policy = policy.Normalize()

	payload, err := timers.MarshalPayload(timers.RetryPayload{
		ReceiptID: receiptID,
		CreatedAt: createdAt,
	})
	if err != nil {
		return fmt.Errorf("marshal retry payload: %w", err)
	}
	_, err = s.engine.CreateTimer(ctx, &store.Timer{
		Kind:      timers.KindRetry,
		ReceiptID: receiptID,
		FireAt:    createdAt, // first attempt is due immediately
		Payload:   payload,
	})
	if err != nil {
		return fmt.Errorf("create initial retry timer: %w", err)
	}
	s.logger.Debug("seeded priority-2 retry timer", "receipt", receiptID, "retry_interval", policy.RetryInterval, "expire", policy.Expire)
	return nil
}

// newRedeliverFunc builds the receipts.Redeliver adapter that re-publishes an
// emergency send to all live WS/SSE connections on each retry attempt. For
// WS-only recipients with no live connection this is a no-op (the message
// stays in the since-cursor and is replayed on reconnect, matching Pushover's
// M6 semantic).
func NewRedeliverFunc(repos store.Repos, publisher hubPublisher, logger *slog.Logger) receipts.Redeliver {
	return func(ctx context.Context, receiptID string, attempt int) error {
		// Resolve receipt -> send -> messages.
		rc, err := repos.Receipts.GetByID(ctx, receiptID)
		if err != nil {
			return fmt.Errorf("redeliver: load receipt %s: %w", receiptID, err)
		}
		sd, err := repos.Sends.GetByID(ctx, rc.SendID)
		if err != nil {
			return fmt.Errorf("redeliver: load send %d: %w", rc.SendID, err)
		}
		msgs, err := repos.Messages.ListBySend(ctx, rc.SendID)
		if err != nil {
			return fmt.Errorf("redeliver: load messages for send %d: %w", rc.SendID, err)
		}
		if publisher != nil {
			publisher.PublishFanout(ctx, sd, msgs)
		}
		logger.Debug("redelivered emergency retry",
			"receipt", receiptID, "send_id", rc.SendID, "attempt", attempt, "recipients", len(msgs))
		return nil
	}
}

// hubPublisher is the narrow interface the redeliver adapter needs from the
// hub. *hub.Hub satisfies it structurally.
type hubPublisher interface {
	PublishFanout(ctx context.Context, send store.Send, msgs []store.Message)
}
