// Package quiethours implements the server-side quiet-hours hold with priority
// bypass (plan todo 14).
//
// # Behaviour
//
// For each per-recipient fan-out row produced by an ingest (todo 8
// CreateFanout), the send path evaluates the recipient's quiet-hours settings:
//
//   - priority >= 1 (Pushover "high"/"emergency") ALWAYS bypasses and is
//     delivered immediately, regardless of the window. This is the manual-QA
//     "failure" case: p2 ignores the hold entirely.
//   - priority <= 0 (-2, -1, 0) inside the recipient's quiet window is HELD:
//     the message row is already persisted, but live delivery to transports is
//     withheld until the window ends.
//   - outside the window, everything delivers immediately.
//
// At window end the held messages are flushed (released). Because the rows are
// already durable in the messages table, a client that reconnects with a since
// cursor receives them naturally via the hub's since-replay (todo 13); the
// flush additionally pushes them to live-connected transports.
//
// Evaluation is per-recipient and per-user independent: each recipient's own
// settings and the injected clock decide hold vs. deliver.
//
// # Time
//
// All time comes from an injected clock (func() time.Time). Window math is
// timezone-aware via time.LoadLocation, so DST transitions are handled by the
// standard library's location rules. No test sleeps on real time.
//
// # Integration seam (for todo 9 fanout)
//
// The send path (todo 8) is not yet committed as of this todo, so this package
// ships the decision/hold/release API plus an in-package scheduler and stops
// there. The wiring point for todo 9 fanout is exactly this, on a per-recipient
// basis after CreateFanout commits:
//
//	mgr := quiethours.New(quiethours.NewMemoryHoldStore(), clock, logger)
//	...
//	for _, m := range fanout.Messages {
//	    recipient, err := repos.Users.GetByID(ctx, m.RecipientUserID)
//	    settings := quiethours.SettingsFromAccount(
//	        recipient.QuietStart, recipient.QuietEnd, recipient.QuietTZ)
//	    switch quiethours.Evaluate(mgr.Now(), settings, send.Priority) {
//	    case quiethours.Hold:
//	        // Live delivery is deferred; the row is already stored.
//	        if err := mgr.Hold(ctx, m.ID, m.RecipientUserID, settings); err != nil {
//	            logger.Warn("quiethours: hold", "err", err)
//	        }
//	    case quiethours.Deliver:
//	        hub.Publish(m.RecipientUserID, hub.StoredMessage{...}) // or fromRow
//	    }
//	}
//
// The window-end flush runs as a background goroutine started at server wiring
// (NOT in this package; todo 9 / the server bootstrap owns the lifecycle). Its
// release callback re-publishes each released hold to the hub:
//
//	go mgr.Run(ctx, time.Minute, func(due []quiethours.HeldMessage) {
//	    for _, h := range due {
//	        msg, _ := repos.Messages.GetByID(ctx, h.MessageID) // or ListSince
//	        sd, _ := repos.Sends.GetByID(ctx, msg.SendID)
//	        hub.Publish(h.RecipientUserID, resolveStoredMessage(msg, sd))
//	    }
//	})
//
// mgr.Run MUST NOT be unit-tested with real time; tests drive mgr.Hold and
// mgr.ReleaseDue directly with an advancing injected clock.
//
// # todo 22 (durable timer engine) seam
//
// The in-memory MemoryHoldStore is intentionally NOT durable: a server restart
// drops pending holds (live delivery of those messages then happens via
// since-replay on the recipient's next connect, so no data is lost, but live
// push at the exact window end is missed). When todo 22 lands, replace
// MemoryHoldStore with a TimerRepo-backed implementation:
//
//   - Add:   timers.Create({Kind: "quiethours", FireAt: releaseAt,
//     Payload: fmt.Sprintf("%d:%d", messageID, recipientUserID)})
//   - Release: the durable worker's ClaimDue(now, n) claims due quiethours
//     timers; the payload carries the message/recipient to re-publish.
//
// Manager.Run is then retired in favour of the shared timer worker. The
// HoldStore interface and the Evaluate/ReleaseAt pure functions do not change,
// so the send-path call site above is stable across the swap.
package quiethours
