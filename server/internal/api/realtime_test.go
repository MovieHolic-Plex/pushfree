package api

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/pushfree/pushfree/internal/store"
)

// TestIngestPublishesLive is the F3 regression for the missing ingest->hub
// fan-out wiring (defect 2). A successful POST /1/messages.json MUST fan the
// just-stored message rows out to the realtime hub so connected WS/SSE
// transports receive them live. Pre-fix, messagesHandler called
// Ingests.Ingest and stopped; no hub.Publish ever fired, so a connected
// client received only keepalives. This test installs a capturing
// LivePublisher and proves (a) PublishFanout fires exactly once per send,
// (b) the row carries the self recipient and a real DB-assigned id (id=0
// would break the hub's subscribe-before-replay de-duplication), and (c)
// the published id equals the durable row the replay path serves.
func TestIngestPublishesLive(t *testing.T) {
	a, base, tok, userKey := ingesterUser(t)
	uid := sessionUserID(t, a, "ingest@example.com")

	var (
		mu      sync.Mutex
		calls   int
		gotSend store.Send
		gotMsgs []store.Message
	)
	a.SetLivePublisher(livePublisherFunc(func(ctx context.Context, send store.Send, msgs []store.Message) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		gotSend = send
		gotMsgs = msgs
	}))

	c := newClient(t)
	status, _, body, raw := postMessages(t, c, base, url.Values{
		"token": {tok}, "user": {userKey}, "message": {"live fanout regression"},
	})
	if status != http.StatusOK || body["status"] != float64(1) {
		t.Fatalf("send failed status=%d body=%s", status, raw)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected exactly 1 PublishFanout call, got %d", calls)
	}
	if len(gotMsgs) != 1 {
		t.Fatalf("expected 1 message row published, got %d", len(gotMsgs))
	}
	if gotMsgs[0].RecipientUserID != uid {
		t.Fatalf("published recipient=%d want self=%d", gotMsgs[0].RecipientUserID, uid)
	}
	if gotMsgs[0].ID == 0 {
		t.Fatalf("message id was not written back by Ingest; live de-dup would break (id=0)")
	}
	if gotMsgs[0].SendID == 0 {
		t.Fatalf("send id not set on published message row")
	}
	if gotSend.Body != "live fanout regression" {
		t.Fatalf("published send body=%q want %q", gotSend.Body, "live fanout regression")
	}

	// The published id must match the durable row the replay path would serve;
	// a mismatch would duplicate or drop the live delivery vs. since-replay.
	rows, err := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected 1 durable row, got %d (%v)", len(rows), err)
	}
	if rows[0].ID != gotMsgs[0].ID {
		t.Fatalf("published id=%d != durable id=%d (replay/dedup mismatch)", gotMsgs[0].ID, rows[0].ID)
	}
}

// livePublisherFunc adapts a function to the LivePublisher interface so tests
// can capture PublishFanout calls without a concrete hub.
type livePublisherFunc func(ctx context.Context, send store.Send, msgs []store.Message)

func (f livePublisherFunc) PublishFanout(ctx context.Context, send store.Send, msgs []store.Message) {
	f(ctx, send, msgs)
}
