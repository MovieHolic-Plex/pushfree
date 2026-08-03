package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pushfree/pushfree/internal/store"
)

// AppRepo is the Postgres implementation of store.AppRepo.
type AppRepo struct{ db DB }

// Create inserts an app row and writes the assigned id back to in.ID.
func (a *AppRepo) Create(ctx context.Context, in *store.App) (int64, error) {
	err := a.db.QueryRowContext(ctx,
		`INSERT INTO apps(user_id, token, name) VALUES ($1, $2, $3) RETURNING id`,
		in.UserID, in.Token, in.Name).Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

func (a *AppRepo) GetByID(ctx context.Context, id int64) (store.App, error) {
	return getApp(ctx, a.db, `SELECT id, user_id, token, name FROM apps WHERE id = $1`, id)
}

func (a *AppRepo) GetByToken(ctx context.Context, token string) (store.App, error) {
	return getApp(ctx, a.db, `SELECT id, user_id, token, name FROM apps WHERE token = $1`, token)
}

func getApp(ctx context.Context, q queryExec, query string, args ...any) (store.App, error) {
	var a store.App
	if err := q.QueryRowContext(ctx, query, args...).Scan(&a.ID, &a.UserID, &a.Token, &a.Name); err != nil {
		return store.App{}, mapErr(err)
	}
	return a, nil
}

// ListByUser returns every app for userID in ascending id (creation) order.
func (a *AppRepo) ListByUser(ctx context.Context, userID int64) ([]store.App, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT id, user_id, token, name FROM apps WHERE user_id = $1 ORDER BY id ASC`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	apps := make([]store.App, 0)
	for rows.Next() {
		var ap store.App
		if err := rows.Scan(&ap.ID, &ap.UserID, &ap.Token, &ap.Name); err != nil {
			return nil, mapErr(err)
		}
		apps = append(apps, ap)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return apps, nil
}

// DeleteByToken deletes the app iff it is owned by userID. Returns
// store.ErrNotFound when no matching row exists (including cross-user
// attempts), so revoke cannot enumerate other users' tokens.
func (a *AppRepo) DeleteByToken(ctx context.Context, userID int64, token string) error {
	res, err := a.db.ExecContext(ctx,
		`DELETE FROM apps WHERE user_id = $1 AND token = $2`, userID, token)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeviceRepo is the Postgres implementation of store.DeviceRepo.
type DeviceRepo struct{ db DB }

// Create inserts a device row and writes the assigned id back to in.ID.
// FCMToken "" maps to NULL via optStr.
func (d *DeviceRepo) Create(ctx context.Context, in *store.Device) (int64, error) {
	err := d.db.QueryRowContext(ctx, `
INSERT INTO devices(user_id, device_id, secret_hash, name, model, os, fcm_token)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id`,
		in.UserID, in.DeviceID, in.SecretHash, in.Name, in.Model, in.OS, optStr(in.FCMToken)).
		Scan(&in.ID)
	if err != nil {
		return 0, fmt.Errorf("device insert: %w", mapErr(err))
	}
	return in.ID, nil
}

// GetByDeviceID loads a device by its device_id, resolving fcm_token NULL ->
// "".
func (d *DeviceRepo) GetByDeviceID(ctx context.Context, deviceID string) (store.Device, error) {
	var (
		dv  store.Device
		fcm sql.NullString
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT id, user_id, device_id, secret_hash, name, model, os, fcm_token FROM devices WHERE device_id = $1`,
		deviceID,
	).Scan(&dv.ID, &dv.UserID, &dv.DeviceID, &dv.SecretHash, &dv.Name, &dv.Model, &dv.OS, &fcm)
	if err != nil {
		return store.Device{}, mapErr(err)
	}
	dv.FCMToken = nullStr(fcm)
	return dv, nil
}

// ClearFCMToken nulls fcm_token for the device matching deviceID. Idempotent:
// a missing device or a device with no token updates zero rows and returns
// nil.
func (d *DeviceRepo) ClearFCMToken(ctx context.Context, deviceID string) error {
	if _, err := d.db.ExecContext(ctx,
		`UPDATE devices SET fcm_token = NULL WHERE device_id = $1`, deviceID); err != nil {
		return mapErr(err)
	}
	return nil
}

// ListByUser returns every device owned by userID in ascending id
// (registration) order, with fcm_token resolved to "" when NULL.
func (d *DeviceRepo) ListByUser(ctx context.Context, userID int64) ([]store.Device, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, user_id, device_id, secret_hash, name, model, os, fcm_token FROM devices WHERE user_id = $1 ORDER BY id ASC`,
		userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	devs := make([]store.Device, 0)
	for rows.Next() {
		var (
			dv  store.Device
			fcm sql.NullString
		)
		if err := rows.Scan(&dv.ID, &dv.UserID, &dv.DeviceID, &dv.SecretHash, &dv.Name, &dv.Model, &dv.OS, &fcm); err != nil {
			return nil, mapErr(err)
		}
		dv.FCMToken = nullStr(fcm)
		devs = append(devs, dv)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return devs, nil
}
