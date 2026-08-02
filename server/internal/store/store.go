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
	Priority     int   // [-2, 2]
	Sound        string
	Title        string
	Body         string
	URL          string
	URLTitle     string
	HTML         bool
	Monospace    bool
	Timestamp    int64 // unix seconds, sender-supplied
	TTL          int64 // seconds
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

// SendRepo covers the ingest path. CreateFanout is the only multi-table write.
type SendRepo interface {
	CreateFanout(ctx context.Context, f *Fanout) (sendID int64, err error)
	GetByID(ctx context.Context, id int64) (Send, error)
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

// ReceiptRepo covers the priority-2 lifecycle record.
type ReceiptRepo interface {
	Create(ctx context.Context, r *Receipt) error
	GetByID(ctx context.Context, id string) (Receipt, error)

	// MarkLastDelivered records the first delivery time on a receipt's
	// last_delivered_at, only while it is still NULL. The full
	// pending->delivered state transition is owned by todo 23; this method
	// is the narrow write the hub's default DeliveryHook needs (todo 13).
	MarkLastDelivered(ctx context.Context, receiptID string, at time.Time) error
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

// Repos bundles every repository interface. Concrete implementations
// (sqlite, later postgres) produce one of these.
type Repos struct {
	Users      UserRepo
	Apps       AppRepo
	Devices    DeviceRepo
	Sends      SendRepo
	Messages   MessageRepo
	Attachments AttachmentRepo
	Receipts   ReceiptRepo
	Quota      QuotaRepo
	Timers     TimerRepo
	Callbacks  CallbackRepo
}
