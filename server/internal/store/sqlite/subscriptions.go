package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// SubscriptionRepo is the SQLite implementation of store.SubscriptionRepo
// (todo 12). Subscription channels live in `subscriptions`; per-(app, user)
// dynamic keys live in `subscription_keys`.
type SubscriptionRepo struct{ db DB }

// subCols is the canonical subscriptions column list + scan order.
const subCols = `id, app_id, owner_user_id, subscription_code, title, created_at`

// Create inserts a subscription channel row. The caller must set AppID,
// OwnerUserID, SubscriptionCode, Title, and CreatedAt; the assigned ID is
// written back to s.
func (r *SubscriptionRepo) Create(ctx context.Context, s *store.Subscription) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
INSERT INTO subscriptions(app_id, owner_user_id, subscription_code, title, created_at)
VALUES (?, ?, ?, ?, ?)`,
		s.AppID, s.OwnerUserID, s.SubscriptionCode, s.Title, rfc3339(s.CreatedAt))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("subscription last insert id: %w", err)
	}
	s.ID = id
	return id, nil
}

// GetByCode returns the subscription channel with the given subscription_code.
func (r *SubscriptionRepo) GetByCode(ctx context.Context, code string) (store.Subscription, error) {
	var (
		s       store.Subscription
		created sql.NullString
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT `+subCols+` FROM subscriptions WHERE subscription_code = ?`, code).
		Scan(&s.ID, &s.AppID, &s.OwnerUserID, &s.SubscriptionCode, &s.Title, &created)
	if err != nil {
		return store.Subscription{}, mapErr(err)
	}
	if t, ok := parseTime(created.String, created.Valid); ok {
		s.CreatedAt = t
	}
	return s, nil
}

// Approve mints (or returns the existing) per-(app, user) dynamic subscriber
// key for the subscription. The UNIQUE(app_id, user_id) constraint makes this
// idempotent and stable: a second approval for the same app+user returns the
// row minted by the first. Collisions on subscribed_key (62^30 space) are
// astronomically unlikely; on a UNIQUE violation the existing row is re-read
// before retrying with a fresh key.
func (r *SubscriptionRepo) Approve(ctx context.Context, subscriptionID, appID, userID int64) (store.SubscriptionKey, error) {
	// Idempotent fast path: a key for (appID, userID) already exists.
	if k, err := r.keyByAppUser(ctx, appID, userID); err == nil {
		return k, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.SubscriptionKey{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		key, err := randomKey30()
		if err != nil {
			return store.SubscriptionKey{}, err
		}
		now := time.Now().UTC()
		res, err := r.db.ExecContext(ctx, `
INSERT INTO subscription_keys(subscription_id, app_id, user_id, subscribed_key, created_at)
VALUES (?, ?, ?, ?, ?)`,
			subscriptionID, appID, userID, key, rfc3339(now))
		if err == nil {
			id, err := res.LastInsertId()
			if err != nil {
				return store.SubscriptionKey{}, fmt.Errorf("subscription key last insert id: %w", err)
			}
			return store.SubscriptionKey{
				ID:             id,
				SubscriptionID: subscriptionID,
				AppID:          appID,
				UserID:         userID,
				SubscribedKey:  key,
				CreatedAt:      now,
			}, nil
		}
		if !store.IsUniqueViolation(err) {
			return store.SubscriptionKey{}, mapErr(err)
		}
		// UNIQUE(app_id, user_id): a concurrent approval won the race, or a
		// subscribed_key collision. Re-read by (appID, userID); if found,
		// return it, otherwise retry with a fresh key.
		if k, err := r.keyByAppUser(ctx, appID, userID); err == nil {
			return k, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return store.SubscriptionKey{}, err
		}
	}
	return store.SubscriptionKey{}, fmt.Errorf("subscription key allocation exhausted after retries")
}

// keyByAppUser returns the subscriber key row for (appID, userID), or
// ErrNotFound if no such approval exists.
func (r *SubscriptionRepo) keyByAppUser(ctx context.Context, appID, userID int64) (store.SubscriptionKey, error) {
	var (
		k       store.SubscriptionKey
		created sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
SELECT id, subscription_id, app_id, user_id, subscribed_key, created_at
FROM subscription_keys WHERE app_id = ? AND user_id = ?`, appID, userID).
		Scan(&k.ID, &k.SubscriptionID, &k.AppID, &k.UserID, &k.SubscribedKey, &created)
	if err != nil {
		return store.SubscriptionKey{}, mapErr(err)
	}
	if t, ok := parseTime(created.String, created.Valid); ok {
		k.CreatedAt = t
	}
	return k, nil
}

// Migrate re-parents a subscription from fromAppID to toAppID and regenerates
// every subscriber key for that subscription: old subscribed_key values are
// replaced (invalidated) and app_id flipped to toAppID, preserving user_id so
// delivery still reaches the same recipient. The whole operation is atomic.
// Returns ErrNotFound if the subscription is absent or not currently parented
// on fromAppID.
func (r *SubscriptionRepo) Migrate(ctx context.Context, subscriptionID, fromAppID, toAppID int64) (int, error) {
	count := 0
	err := inTx(ctx, r.db, func(q queryExec) error {
		// Re-parent the channel first; RowsAffected==0 means the subscription
		// is absent or already on a different app.
		res, err := q.ExecContext(ctx,
			`UPDATE subscriptions SET app_id = ? WHERE id = ? AND app_id = ?`,
			toAppID, subscriptionID, fromAppID)
		if err != nil {
			return mapErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		// Collect the keys to regenerate, then update them. Reading all rows
		// before mutating avoids cursor-while-writing issues.
		rows, err := q.QueryContext(ctx,
			`SELECT id FROM subscription_keys WHERE subscription_id = ? AND app_id = ?`,
			subscriptionID, fromAppID)
		if err != nil {
			return mapErr(err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return mapErr(err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapErr(err)
		}
		for _, id := range ids {
			key, err := randomKey30()
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx,
				`UPDATE subscription_keys SET app_id = ?, subscribed_key = ? WHERE id = ?`,
				toAppID, key, id); err != nil {
				return mapErr(err)
			}
			count++
		}
		return nil
	})
	return count, err
}

// subKeyAlphabet is the [A-Za-z0-9] space from which subscribed_user_key
// identifiers are drawn. randomKey30 mirrors the api package's newUserKey
// (30-char [A-Za-z0-9] from crypto/rand with rejection sampling) so the store
// layer mints keys without importing the api package (which would invert the
// dependency direction).
const subKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomKey30 returns a 30-char [A-Za-z0-9] identifier sourced from
// crypto/rand with rejection sampling so the distribution is unbiased.
func randomKey30() (string, error) {
	const n = 30
	const slots = len(subKeyAlphabet) // 62
	// Reject the trailing biased tail (248..255 for a 62-symbol alphabet).
	const limit = (256 / slots) * slots
	buf := make([]byte, n)
	out := make([]byte, 0, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("subscription key: read random: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, subKeyAlphabet[int(b)%slots])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
