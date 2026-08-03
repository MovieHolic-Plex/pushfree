package api

import (
	"context"

	"github.com/pushfree/pushfree/internal/store"
)

// LivePublisher is the seam the realtime hub plugs into so a successful
// /1/messages.json ingest can fan a just-stored message out to every live
// WS/SSE transport of its recipient (the product's core "push without FCM"
// surface). It mirrors the AckHook pattern: a setter (not a constructor
// param) so server wiring can install the hub after the Accounts group is
// built without changing the constructor signature owned by todo 6.
//
// The interface speaks only in store types so the api package does not import
// the hub package; *hub.Hub satisfies it structurally via its PublishFanout
// method. nil (the default) means the ingest path skips live fan-out and the
// message is still durable (served by since-replay on the next connect), so
// wiring is optional and tests can run without a hub.
type LivePublisher interface {
	// PublishFanout live-delivers already-stored message rows for one send.
	// send carries the content fields; msgs carries each recipient user id
	// plus the DB-assigned message id. Best-effort: errors are owned by the
	// implementation and must not propagate to the ingest caller (the send is
	// already committed).
	PublishFanout(ctx context.Context, send store.Send, msgs []store.Message)
}

// SetLivePublisher installs the realtime fan-out hook fired after a successful
// message ingest. nil (the default) disables live push; messages remain
// durable and are served by the since-replay pull/WS path on connect.
func (a *Accounts) SetLivePublisher(p LivePublisher) { a.livePublisher = p }
