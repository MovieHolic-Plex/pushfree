// Package store defines the pushfree persistence boundary.
//
// This file holds only implementation-agnostic types and repository
// interfaces. The concrete SQLite implementation lives under
// internal/store/sqlite. A later todo (45) adds a Postgres implementation
// behind these same interfaces, so signatures intentionally avoid any
// database-specific types: IDs are int64, temporal columns are time.Time,
// and nullable TEXT columns are exposed as the empty string (NULL == "").
//
// Time convention (documented for the 45-Postgres port):
//   - Columns that are logical instants (created_at, delivered_at, fire_at,
//     expires_at, *_at) are time.Time in Go and stored as RFC3339 text in
//     SQLite. A nullable instant is *time.Time (nil == NULL).
//   - Columns that are NOT instants (quiet_start "HH:MM", quiet_tz IANA name,
//     quota period "YYYY-MM") are plain strings.
//   - Nullable non-time TEXT (tag, callback_url, receipt_id, device_filter,
//     receipt_id pointers) uses "" to mean NULL.
package store

import (
	"context"
	"errors"
	"time"
)

// Common sentinel errors. Concrete drivers wrap their engine-specific errors
// onto these so callers can branch without importing the driver.
var (
	// ErrNotFound is returned by Get* when no row matches.
	ErrNotFound = errors.New("store: not found")
	// ErrUniqueViolation is returned by Create/Update when a UNIQUE or
	// PRIMARY KEY constraint fails. Use errors.Is to detect it.
	ErrUniqueViolation = errors.New("store: unique constraint violation")
)

// IsUniqueViolation reports whether err represents a UNIQUE/PK constraint
// failure. Both sentinel wrapping and the raw driver text are accepted so the
// API layer (todo 6/8) can map it to HTTP 409 without importing the driver.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrUniqueViolation)
}

// User is a pushfree account. QuietStart/QuietEnd are "" when unset.
type User struct {
	ID         int64
	Email      string
	PassHash   string
	Role       string // "user" | "admin"
	UserKey    string // exactly 30 chars
	QuietStart string // "HH:MM" or ""
	QuietEnd   string // "HH:MM" or ""
	QuietTZ    string // IANA name, defaults to "UTC"
	CreatedAt  time.Time
}

// App is an integration token owned by a user.
type App struct {
	ID     int64
	UserID int64
	Token  string // exactly 30 chars
	Name   string
}

// Device is a registered client belonging to a user. FCMToken is "" if unset.
type Device struct {
	ID         int64
	UserID     int64
	DeviceID   string
	SecretHash string
	Name       string
	Model      string
	OS         string
	FCMToken   string // "" if NULL
}

// Send is a single API ingest call (the parent of fan-out). Tag, CallbackURL
// and ReceiptID are "" when NULL.
type Send struct {
	ID           int64
	AppID        int64
	SenderUserID int64
	Priority     int // [-2, 2]
	Sound        string
	Title        string
	Body         string
	URL          string
	URLTitle     string
	HTML         bool
	Monospace    bool
	Timestamp    int64  // unix seconds, sender-supplied
	TTL          int64  // seconds
	Tag          string // "" if NULL
	Encrypted    bool
	CallbackURL  string // "" if NULL
	ReceiptID    string // "" if NULL
	CreatedAt    time.Time
}

// Message is one per-recipient fan-out row. DeviceFilter is "" if NULL.
// DeliveredAt is nil until the hub confirms transport acceptance.
type Message struct {
	ID              int64
	SendID          int64
	RecipientUserID int64
	DeviceFilter    string     // "" if NULL
	DeliveredAt     *time.Time // nil if not yet delivered
	CreatedAt       time.Time
}

// Attachment stores the (single) attachment for a send, 1:1.
type Attachment struct {
	ID           int64
	SendID       int64
	ContentType  string
	Data         []byte
	DownloadedAt *time.Time // nil if not yet downloaded
}

// Receipt is the emergency-priority lifecycle record, 1:1 with a send.
// All *time.Time fields are nil until the corresponding event occurs.
type Receipt struct {
	ID                   string // exactly 30 chars
	SendID               int64
	State                string // pending|delivered|acknowledged|expired|canceled
	Tag                  string // "" if NULL
	RetryCount           int
	ExpiresAt            *time.Time
	AcknowledgedAt       *time.Time
	AcknowledgedBy       string // "" if NULL
	AcknowledgedByDevice string // "" if NULL
	LastDeliveredAt      *time.Time
	CalledBackAt         *time.Time
	ExpiredAt            *time.Time
	CanceledAt           *time.Time
}

// QuotaCounter is a per-user, per-month send counter.
type QuotaCounter struct {
	UserID int64
	Period string // "YYYY-MM"
	Count  int64
}

// Timer is a durable, claimable scheduled callback. ReceiptID is "" if NULL.
// ClaimedAt is nil until a worker atomically claims the row.
type Timer struct {
	ID        int64
	Kind      string // retry|expire|callback|quiethours
	ReceiptID string // "" if NULL
	FireAt    time.Time
	Payload   string
	ClaimedAt *time.Time // nil if unclaimed
}

// Callback is a pending receipt-delivery webhook. NextAttemptAt is nil if
// none is scheduled.
type Callback struct {
	ID            int64
	ReceiptID     string
	URL           string
	State         string
	NextAttemptAt *time.Time
	Attempts      int
}

// DLQ is a dead-letter record for a callback that keeps failing. Owned by the
// callback worker (todo 25); exposed here so the table has coverage.
type DLQ struct {
	ID         int64
	CallbackID int64
	LastError  string
	At         time.Time
	Attempts   int
}

// Group is a named delivery group owned by a user (todo 9). GroupKey is
// exactly 30 chars [A-Za-z0-9] -- the SAME format as UserKey, so the send
// path cannot distinguish a user_key from a group_key; the store resolves
// each via SendRepo.ResolveRecipients. Sending to a group fans out one
// messages row per member (H1 sends-parent model).
type Group struct {
	ID        int64
	UserID    int64  // owner
	GroupKey  string // exactly 30 chars
	Name      string
	Memo      string // <= 200 chars
	CreatedAt time.Time
}

// UserRepo covers account lookup and creation.
type UserRepo interface {
	Create(ctx context.Context, u *User) (int64, error)
	// CreateBootstrap inserts u, assigning role="admin" if it is the first
	// user and "user" otherwise. The role is computed from the current user
	// count inside the same atomic statement that inserts, so two concurrent
	// first-time registrations cannot both become admin (SQLite serializes
	// writers; the CASE subquery runs under the write lock). It overwrites
	// u.Role and u.ID. Email/PassHash/UserKey/CreatedAt must be set by the
	// caller; QuietTZ defaults to "UTC" when empty.
	CreateBootstrap(ctx context.Context, u *User) (int64, error)
	// UpdateQuietHours persists a user's quiet-hours window. Pass "" for
	// quietStart/quietEnd to clear the window (NULL); tz must be non-empty
	// and defaults to "UTC" when empty. Returns ErrNotFound if id is absent.
	UpdateQuietHours(ctx context.Context, id int64, quietStart, quietEnd, tz string) error
	GetByID(ctx context.Context, id int64) (User, error)
	GetByEmail(ctx context.Context, email string) (User, error)
	GetByUserKey(ctx context.Context, userKey string) (User, error)
}

// AppRepo covers integration-token management.
type AppRepo interface {
	Create(ctx context.Context, a *App) (int64, error)
	GetByID(ctx context.Context, id int64) (App, error)
	GetByToken(ctx context.Context, token string) (App, error)
	// ListByUser returns every app owned by userID, ordered by id ascending
	// (creation order). Used by GET /1/apps to surface token values to the
	// owner (todo 7).
	ListByUser(ctx context.Context, userID int64) ([]App, error)
	// DeleteByToken deletes the app with the given token iff it belongs to
	// userID. It returns ErrNotFound if no such (user, token) row exists, so
	// a cross-user revoke attempt is indistinguishable from a missing token
	// (no enumeration). Used by DELETE /1/apps/{token} (todo 7) and is the
	// revoke that makes a token fail ValidateAppToken on the send path.
	DeleteByToken(ctx context.Context, userID int64, token string) error
}

// DeviceRepo covers client registration.
type DeviceRepo interface {
	Create(ctx context.Context, d *Device) (int64, error)
	GetByDeviceID(ctx context.Context, deviceID string) (Device, error)

	// ClearFCMToken nulls the fcm_token of the device with the given
	// device_id. Called by the FCM delivery channel (todo 16) when FCM
	// reports the token as UNREGISTERED or INVALID_ARGUMENT, so the device
	// must re-register before it can receive FCM again. It is not an error
	// if the device has no token or does not exist (idempotent clear).
	ClearFCMToken(ctx context.Context, deviceID string) error

	// ListByUser returns every device owned by userID in ascending id
	// (registration) order, with fcm_token resolved to "" when NULL. Used by
	// POST /1/users/validate.json (todo 11) to surface the recipient's
	// registered device names.
	ListByUser(ctx context.Context, userID int64) ([]Device, error)
}

// Fanout is the atomic unit of an API ingest: one send row, N per-recipient
// message rows, and an optional receipt (priority-2) row, all committed in a
// single transaction. This is the H1 "sends-parent" model: a receipt belongs
// to the API send, not to any individual recipient message.
type Fanout struct {
	Send     Send
	Messages []Message
	Receipt  *Receipt // nil to skip
}

// IngestInput is the full atomic write for one POST /1/messages.json call
// (todo 8): the parent send, its per-recipient fan-out messages, an optional
// priority-2 receipt placeholder, and an optional attachment. All non-nil
// parts are committed in one transaction by IngestRepo.Ingest. It mirrors
// Fanout but adds the attachment (the messages.json handler persists the
// attachment atomically with the send).
type IngestInput struct {
	Send       Send
	Messages   []Message
	Receipt    *Receipt    // nil unless priority == 2
	Attachment *Attachment // nil if no attachment
}

// SendRepo covers the ingest path. CreateFanout is the only multi-table write.
type SendRepo interface {
	CreateFanout(ctx context.Context, f *Fanout) (sendID int64, err error)
	GetByID(ctx context.Context, id int64) (Send, error)

	// ResolveRecipients expands a list of 30-char keys (user_key or group_key)
	// into concrete recipient user IDs to fan out to. This is the SINGLE
	// send-time lookup path (todo 9): the caller cannot tell a user_key from a
	// group_key, so the store resolves each. A user_key yields one recipient;
	// a group_key yields its members. A key matching neither a user nor a
	// group returns ErrNotFound (the caller maps this to 404).
	ResolveRecipients(ctx context.Context, keys []string) ([]int64, error)
}

// IngestRepo performs the atomic write for one POST /1/messages.json call
// (todo 8): one send, per-recipient messages, an optional priority-2 receipt,
// and an optional attachment, all committed together. It is distinct from
// SendRepo.CreateFanout because the messages.json ingest path also persists
// the attachment atomically; the concrete implementation lives in
// internal/store/sqlite/ingest.go so it never edits the worker-owned
// send_message.go/receipt.go files.
type IngestRepo interface {
	Ingest(ctx context.Context, in *IngestInput) (sendID int64, err error)
}

// MessageRepo covers per-recipient delivery queries.
type MessageRepo interface {
	ListSince(ctx context.Context, recipientUserID int64, afterID int64, limit int) ([]Message, error)

	// MarkDelivered records the first transport-accepted delivery time on a
	// message row. It is a no-op when delivered_at is already set, so it is
	// safe to call on every replay/redelivery. Backs the hub DeliveryHook
	// (todo 13); todo 23 extends delivery confirmation into the receipt state
	// machine.
	MarkDelivered(ctx context.Context, messageID int64, at time.Time) error

	// MaxID returns the highest message id for a recipient, or 0 if the
	// recipient has no messages. It is the high-water mark the hub reports in
	// the WS/SSE "open" frame (todo 13).
	MaxID(ctx context.Context, recipientUserID int64) (int64, error)
}

// AttachmentRepo covers the 1:1 send attachment.
type AttachmentRepo interface {
	Create(ctx context.Context, a *Attachment) (int64, error)
	GetBySendID(ctx context.Context, sendID int64) (Attachment, error)
}

// ReceiptSweepResult reports how many rows each table lost in one receipt
// GC pass (todo 23). Receipts is the count of receipt rows deleted; the other
// fields are the dependent rows (timers/callbacks/dlq) that referenced them
// and were removed in the same transaction to satisfy the receipts FK.
type ReceiptSweepResult struct {
	Receipts  int64
	Timers    int64
	Callbacks int64
	DLQ       int64
}

// ReceiptRepo covers the priority-2 lifecycle record.
type ReceiptRepo interface {
	Create(ctx context.Context, r *Receipt) error
	GetByID(ctx context.Context, id string) (Receipt, error)

	// MarkLastDelivered records the first delivery time on a receipt's
	// last_delivered_at, only while it is still NULL. The full
	// pending->delivered state transition is owned by todo 23; this method
	// is the narrow write the hub's default DeliveryHook needs (todo 13).
	MarkLastDelivered(ctx context.Context, receiptID string, at time.Time) error

	// Acknowledge atomically transitions a pending/delivered receipt to the
	// acknowledged terminal state, recording who/when (todo 23). It is
	// idempotent: an already-acknowledged receipt is left untouched (the
	// original acknowledged_at/_by/_by_device are preserved) and returns nil.
	// Acknowledging an expired/canceled receipt is an illegal forward
	// transition and is a no-op returning nil, so the HTTP ack endpoint treats
	// a race with expiry/cancel as success rather than an error. Stopping
	// retries is implied: the scheduler (todo 21) observes the terminal state
	// and emits EventDone, so no retry timer is re-armed.
	Acknowledge(ctx context.Context, id, acknowledgedBy, acknowledgedByDevice string, at time.Time) error

	// SweepReceipts garbage-collects receipts whose retention window has
	// elapsed (default 7 days, the Pushover receipt query window), cascading
	// the delete to dependent timers, callbacks and callback-DLQ rows (todo
	// 23). A receipt is eligible when it is terminal
	// (acknowledged/expired/canceled) AND its terminal timestamp is older
	// than now-retention, OR when it is still pending/delivered but past
	// expires_at+retention (an unacked emergency whose scheduler-driven
	// expiry has not been recorded). now is injectable so the boundary is
	// tested without sleeping. Returns per-table delete counts.
	SweepReceipts(ctx context.Context, now time.Time, retention time.Duration) (ReceiptSweepResult, error)
}

// QuotaRepo covers the monthly send counter.
type QuotaRepo interface {
	Increment(ctx context.Context, userID int64, period string, delta int64) (int64, error)
	Get(ctx context.Context, userID int64, period string) (QuotaCounter, error)
}

// TimerRepo covers durable scheduled work and its atomic claim.
type TimerRepo interface {
	Create(ctx context.Context, t *Timer) (int64, error)
	// ClaimDue atomically claims up to limit due, unclaimed timers whose
	// fire_at <= now, marking claimed_at and returning the claimed rows.
	// Each timer is returned to exactly one caller.
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]Timer, error)
}

// CallbackRepo covers webhook delivery and its dead-letter queue.
type CallbackRepo interface {
	Create(ctx context.Context, c *Callback) (int64, error)
	GetByID(ctx context.Context, id int64) (Callback, error)
	CreateDLQ(ctx context.Context, d *DLQ) (int64, error)
	ListDLQForCallback(ctx context.Context, callbackID int64) ([]DLQ, error)
}

// GroupRepo covers delivery-group CRUD and membership (todo 9). Member
// identities cross the boundary as user_ids; the API layer resolves
// user_key<->user_id so the store never parses key format.
type GroupRepo interface {
	Create(ctx context.Context, g *Group) (int64, error)
	GetByID(ctx context.Context, id int64) (Group, error)
	// GetByKey returns the group with the given group_key. Used by the send
	// resolution path; returns ErrNotFound if no such group exists.
	GetByKey(ctx context.Context, key string) (Group, error)
	ListByOwner(ctx context.Context, ownerID int64) ([]Group, error)
	// Update changes the name and memo of the group with the given id.
	// Returns ErrNotFound if id is absent.
	Update(ctx context.Context, id int64, name, memo string) error
	// Delete removes the group and its members. Returns ErrNotFound if id is
	// absent. Cascade is explicit (group_members has no ON DELETE CASCADE) so
	// the implementation controls the order inside one transaction.
	Delete(ctx context.Context, id int64) error
	// SetMembers adds and removes members (by user_id) in one transaction.
	// Adds use INSERT OR IGNORE so a duplicate add is a no-op; removes of a
	// non-member are a no-op.
	SetMembers(ctx context.Context, groupID int64, add, remove []int64) error
	// ListMemberIDs returns the user_ids of the members of groupID.
	ListMemberIDs(ctx context.Context, groupID int64) ([]int64, error)
	// ListMemberKeys returns the user_keys of the members of groupID, for
	// surfacing membership in the groups API response.
	ListMemberKeys(ctx context.Context, groupID int64) ([]string, error)
}

// Subscription is a discoverable "subscribe to my app" channel owned by an
// app (todo 12). SubscriptionCode is exactly 30 chars [A-Za-z0-9]. Title is
// the human-readable channel name (may be "").
type Subscription struct {
	ID               int64
	AppID            int64
	OwnerUserID      int64
	SubscriptionCode string // exactly 30 chars
	Title            string
	CreatedAt        time.Time
}

// SubscriptionKey is one approved per-(app, user) dynamic key (todo 12). The
// (AppID, UserID) pair is unique so approval is stable: re-approving the same
// app+user returns the same key. SubscribedKey resolves to UserID in the send
// path (ResolveRecipients), behaving exactly like a user_key (same 30-char
// [A-Za-z0-9] format).
type SubscriptionKey struct {
	ID             int64
	SubscriptionID int64
	AppID          int64
	UserID         int64
	SubscribedKey  string // exactly 30 chars
	CreatedAt      time.Time
}

// SubscriptionRepo covers the subscription-code + dynamic-key lifecycle (todo
// 12). Member identities cross the boundary as user_ids; the API layer
// resolves user_key <-> user_id.
type SubscriptionRepo interface {
	Create(ctx context.Context, s *Subscription) (int64, error)
	// GetByCode returns the subscription with the given subscription_code.
	// Used by the authorize and migrate paths; returns ErrNotFound if absent.
	GetByCode(ctx context.Context, code string) (Subscription, error)
	// Approve mints (or returns, if (appID, userID) already exists) the
	// per-app+user dynamic key for the subscription. It is idempotent and
	// stable: re-approving the same app+user returns the SAME key. The key is
	// different per app. appID is the app the subscription currently belongs
	// to (the owner may migrate it later).
	Approve(ctx context.Context, subscriptionID, appID, userID int64) (SubscriptionKey, error)
	// Migrate re-parents a subscription from fromAppID to toAppID and
	// regenerates every subscriber key for that subscription (old keys
	// invalidated, new keys minted for toAppID), preserving each user_id. It
	// returns the count of remapped keys, or ErrNotFound if the subscription
	// is absent or not currently parented on fromAppID. The caller must have
	// validated ownership of both apps.
	Migrate(ctx context.Context, subscriptionID, fromAppID, toAppID int64) (int, error)
}

// Repos bundles every repository interface. Concrete implementations
// (sqlite, later postgres) produce one of these.
type Repos struct {
	Users         UserRepo
	Apps          AppRepo
	Devices       DeviceRepo
	Sends         SendRepo
	Messages      MessageRepo
	Attachments   AttachmentRepo
	Receipts      ReceiptRepo
	Quota         QuotaRepo
	Timers        TimerRepo
	Callbacks     CallbackRepo
	Ingests       IngestRepo
	Groups        GroupRepo
	Subscriptions SubscriptionRepo
}
