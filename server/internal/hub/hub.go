package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// Protocol constants. See doc.go and research W2-ws.md for the rationale.
const (
	// keepaliveIntervalDefault is the production keepalive cadence (seconds).
	// It sits under nginx's default 60s idle timeout and common NAT/carrier
	// 60-120s connection drops. Injectable via Options.KeepaliveInterval.
	keepaliveIntervalDefault = 45 * time.Second

	// readTimeoutDefault bounds the WS login frame read. The Open Client is
	// otherwise silent after login, so post-login liveness is detected via
	// keepalive write failure + conn.CloseRead rather than a hard read
	// deadline (a blanket 77s read deadline would close every idle client).
	readTimeoutDefault = 77 * time.Second

	// messagePageLimit caps a since-replay / messages-pull page, oldest first.
	messagePageLimit = 100

	// inboxCap buffers live messages per connection so a momentary transport
	// stall does not block Publish. If exceeded the connection is force-closed
	// (the client reconnects and replays via since) rather than dropping data.
	inboxCap = 256

	// wsCloseAuth is the application close code used for all auth failures
	// (unknown device, wrong secret, bad login frame). It is in the RFC 6455
	// application-defined range 4000-4999.
	wsCloseAuth = 4001
)

// StoredMessage is a fully-resolved message ready for client delivery. It
// joins a per-recipient fan-out row (id) with its parent send's content so a
// transport serializes a single self-contained frame.
type StoredMessage struct {
	ID              int64  `json:"id"`
	SendID          int64  `json:"send_id"`
	RecipientUserID int64  `json:"-"` // never serialized; identifies the user
	Priority        int    `json:"priority"`
	Sound           string `json:"sound,omitempty"`
	Title           string `json:"title,omitempty"`
	Body            string `json:"message"`
	URL             string `json:"url,omitempty"`
	URLTitle        string `json:"url_title,omitempty"`
	HTML            bool   `json:"html,omitempty"`
	Monospace       bool   `json:"monospace,omitempty"`
	Timestamp       int64  `json:"timestamp"`
	TTL             int64  `json:"ttl,omitempty"`
	Tag             string `json:"tag,omitempty"`
	Encrypted       bool   `json:"encrypted,omitempty"`
}

// fromRow joins a stored message row with its parent send into a StoredMessage.
func fromRow(m store.Message, s store.Send) StoredMessage {
	return StoredMessage{
		ID:              m.ID,
		SendID:          m.SendID,
		RecipientUserID: m.RecipientUserID,
		Priority:        s.Priority,
		Sound:           s.Sound,
		Title:           s.Title,
		Body:            s.Body,
		URL:             s.URL,
		URLTitle:        s.URLTitle,
		HTML:            s.HTML,
		Monospace:       s.Monospace,
		Timestamp:       s.Timestamp,
		TTL:             s.TTL,
		Tag:             s.Tag,
		Encrypted:       s.Encrypted,
	}
}

// MessageStore resolves stored fan-out rows into deliverable StoredMessages
// (the since-replay path) and reports the per-user high-water mark (the "open"
// frame). The default implementation, repoMessageStore, wraps store.Repos.
// Production callers publish already-resolved StoredMessages via Hub.Publish;
// only replay needs this read/resolve.
type MessageStore interface {
	ListMessages(ctx context.Context, userID, afterID int64, limit int) ([]StoredMessage, error)
	MaxMessageID(ctx context.Context, userID int64) (int64, error)
}

// repoMessageStore adapts store.MessageRepo + store.SendRepo to MessageStore.
// It resolves one send per message (todo 8 will likely add a batched join; the
// per-row lookup is correct and bounded by messagePageLimit for replay).
type repoMessageStore struct {
	msgs  store.MessageRepo
	sends store.SendRepo
}

func (s repoMessageStore) ListMessages(ctx context.Context, userID, afterID int64, limit int) ([]StoredMessage, error) {
	rows, err := s.msgs.ListSince(ctx, userID, afterID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]StoredMessage, 0, len(rows))
	for _, m := range rows {
		sd, err := s.sends.GetByID(ctx, m.SendID)
		if err != nil {
			return nil, err
		}
		out = append(out, fromRow(m, sd))
	}
	return out, nil
}

func (s repoMessageStore) MaxMessageID(ctx context.Context, userID int64) (int64, error) {
	return s.msgs.MaxID(ctx, userID)
}

// DeliveryHook is invoked after a transport accepts (successfully writes) a
// message. "Accept" means a WS text-frame write succeeded or an SSE write+
// flush succeeded. The default implementation, repoDeliveryHook, records
// delivered_at on the message row and last_delivered_at on its receipt (if
// any). Todo 23 replaces this with the receipt-state-machine-aware confirmer
// (pending->delivered) that also drives retries; the interface is the seam.
type DeliveryHook interface {
	OnDelivered(ctx context.Context, msg StoredMessage) error
}

// repoDeliveryHook writes delivery timestamps through the repos. now is
// injectable so tests can assert deterministic timestamps; it defaults to
// time.Now in New.
type repoDeliveryHook struct {
	msgs     store.MessageRepo
	sends    store.SendRepo
	receipts store.ReceiptRepo
	now      func() time.Time
}

// DefaultDeliveryHook returns the standard DeliveryHook: it records
// delivered_at on the message row and last_delivered_at on its receipt (if
// any). Exported so callers can wrap it (e.g. with metrics or a completion
// signal) while keeping the default behaviour; New uses it when
// Options.DeliveryHook is nil.
func DefaultDeliveryHook(repos store.Repos, now func() time.Time) DeliveryHook {
	if now == nil {
		now = time.Now
	}
	return repoDeliveryHook{
		msgs: repos.Messages, sends: repos.Sends, receipts: repos.Receipts, now: now,
	}
}

func (h repoDeliveryHook) OnDelivered(ctx context.Context, msg StoredMessage) error {
	at := h.now()
	if err := h.msgs.MarkDelivered(ctx, msg.ID, at); err != nil {
		return err
	}
	// last_delivered_at lives on the send-level receipt (H1), so resolve the
	// receipt id through the parent send. A send without a receipt (priority
	// != 2) simply has nothing to touch.
	sd, err := h.sends.GetByID(ctx, msg.SendID)
	if err != nil {
		return err
	}
	if sd.ReceiptID == "" {
		return nil
	}
	return h.receipts.MarkLastDelivered(ctx, sd.ReceiptID, at)
}

// SessionUserResolver resolves the authenticated account from an HTTP request
// for device registration. The production implementation is todo 6's session
// middleware (httpOnly cookie -> user id); tests inject a stub. This is the
// wiring seam: device login is the only hub handler that needs a session, and
// the real resolver is plugged in at server wiring time (todo 8), not here.
type SessionUserResolver interface {
	ResolveUserID(r *http.Request) (userID int64, ok bool)
}

// Options configures a Hub. Zero-value fields take the documented defaults.
type Options struct {
	// KeepaliveInterval is the WS/SSE keepalive cadence. MUST be injectable
	// (tests use a short value). Defaults to 45s.
	KeepaliveInterval time.Duration
	// ReadTimeout bounds the WS login-frame read. Defaults to 77s.
	ReadTimeout time.Duration
	// Clock returns "now"; defaults to time.Now. Lets the delivery hook write
	// deterministic timestamps.
	Clock func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// MessageStore overrides the default repo-backed store. Leave nil for the
	// default (wraps store.Repos).
	MessageStore MessageStore
	// DeliveryHook overrides the default repo-backed hook. Leave nil for the
	// default.
	DeliveryHook DeliveryHook
}

// Hub is the single-node in-process fan-out. It maps a user id to the set of
// live transports subscribed for that user and delivers published messages to
// all of them. It is safe for concurrent use.
type Hub struct {
	devs      store.DeviceRepo
	store     MessageStore
	hook      DeliveryHook
	sessions  SessionUserResolver
	keepalive time.Duration
	readTtl   time.Duration
	log       *slog.Logger

	mu    sync.RWMutex
	users map[int64]map[*subscription]struct{}

	closeOnce sync.Once
	done      chan struct{}
}

// subscription is one live connection's mailbox. ch carries live messages
// (buffered); close is signaled by closing done.
type subscription struct {
	userID int64
	ch     chan StoredMessage
}

// New builds a Hub backed by repos. The hub holds only the narrow repo slices
// it needs (Devices, Messages, Sends, Receipts). Options supplies the
// injectable timing and the optional store/hook overrides.
func New(repos store.Repos, sessions SessionUserResolver, opts Options) *Hub {
	if opts.KeepaliveInterval <= 0 {
		opts.KeepaliveInterval = keepaliveIntervalDefault
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = readTimeoutDefault
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MessageStore == nil {
		opts.MessageStore = repoMessageStore{msgs: repos.Messages, sends: repos.Sends}
	}
	if opts.DeliveryHook == nil {
		opts.DeliveryHook = DefaultDeliveryHook(repos, opts.Clock)
	}
	return &Hub{
		devs:      repos.Devices,
		store:     opts.MessageStore,
		hook:      opts.DeliveryHook,
		sessions:  sessions,
		keepalive: opts.KeepaliveInterval,
		readTtl:   opts.ReadTimeout,
		log:       opts.Logger,
		users:     make(map[int64]map[*subscription]struct{}),
		done:      make(chan struct{}),
	}
}

// Close signals every live transport to wind down. It is the graceful-shutdown
// primitive (todo 18 wires SIGTERM to it) and also makes tests deterministic:
// closing the hub before httptest.Server.Close() ensures streaming handlers
// exit instead of blocking on client-disconnect detection, which is unreliable
// for streaming responses on some platforms. Idempotent.
func (h *Hub) Close() {
	h.closeOnce.Do(func() { close(h.done) })
}

// PublishFanout resolves and live-publishes already-stored message rows for
// one send to each row's recipient. It is the seam the messages.json ingest
// handler calls after a successful Ingest so connected WS/SSE clients receive
// the message in real time, in addition to the durable store row that
// since-replay serves on the next connect. send carries the content fields;
// each message row carries the recipient user id and the DB-assigned message
// id that the subscribe-before-replay write loop de-duplicates against. A
// publish with no live subscriber is a no-op (the row is already durable),
// so this is safe to call unconditionally after Ingest.
func (h *Hub) PublishFanout(ctx context.Context, send store.Send, msgs []store.Message) {
	for _, m := range msgs {
		h.Publish(m.RecipientUserID, fromRow(m, send))
	}
}

// Publish delivers msg to every live transport of userID. It is non-blocking:
// if a connection's inbox is full the connection is force-closed (it will
// reconnect and replay via since) so a slow client cannot stall the publisher
// or silently drop the message.
func (h *Hub) Publish(userID int64, msg StoredMessage) {
	h.mu.RLock()
	set := h.users[userID]
	subs := make([]*subscription, 0, len(set))
	for s := range set {
		subs = append(subs, s)
	}
	h.mu.RUnlock()

	for _, s := range subs {
		select {
		case s.ch <- msg:
		default:
			// Inbox full -> the client is stalling. Close its mailbox so its
			// write loop exits and the transport tears down; the client
			// reconnects and since-replays. Dropping is avoided: the message
			// is durable and will be replayed.
			h.log.Warn("hub: inbox full, dropping connection", "user_id", userID, "msg_id", msg.ID)
			h.unsubscribe(s)
		}
	}
}

// subscribe registers a mailbox for userID. The caller MUST call unsubscribe
// when the transport closes. Subscribe happens BEFORE since-replay so a
// Publish during replay lands in the inbox and is de-duplicated by id in the
// write loop (no lost, no duplicate messages).
func (h *Hub) subscribe(userID int64) *subscription {
	s := &subscription{userID: userID, ch: make(chan StoredMessage, inboxCap)}
	h.mu.Lock()
	set := h.users[userID]
	if set == nil {
		set = make(map[*subscription]struct{})
		h.users[userID] = set
	}
	set[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *Hub) unsubscribe(s *subscription) {
	h.mu.Lock()
	set := h.users[s.userID]
	if set != nil {
		delete(set, s)
		if len(set) == 0 {
			delete(h.users, s.userID)
		}
	}
	h.mu.Unlock()
}

// fireHook records delivery via the hook. Hook errors are logged, not
// propagated: the message was already accepted by the transport, so a
// side-effect failure must not abort the connection.
func (h *Hub) fireHook(ctx context.Context, msg StoredMessage) {
	if h.hook == nil {
		return
	}
	if err := h.hook.OnDelivered(ctx, msg); err != nil {
		h.log.Warn("hub: delivery hook failed", "msg_id", msg.ID, "err", err)
	}
}

// marshalMessage renders the {"type":"message", ...} WS/SSE line for m.
func marshalMessage(m StoredMessage) []byte {
	b, _ := json.Marshal(struct {
		Type string `json:"type"`
		StoredMessage
	}{"message", m})
	return b
}

// keepaliveFrame is the WS keepalive line, precomputed.
var keepaliveFrame = []byte(`{"type":"keepalive"}`)
