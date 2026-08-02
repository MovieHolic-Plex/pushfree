package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pushfree/pushfree/internal/store"
)

// AppRepo is the SQLite implementation of store.AppRepo.
type AppRepo struct{ db DB }

func (a *AppRepo) Create(ctx context.Context, in *store.App) (int64, error) {
	res, err := a.db.ExecContext(ctx,
		`INSERT INTO apps(user_id, token, name) VALUES (?, ?, ?)`,
		in.UserID, in.Token, in.Name)
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("app last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

func (a *AppRepo) GetByID(ctx context.Context, id int64) (store.App, error) {
	return getApp(ctx, a.db, `SELECT id, user_id, token, name FROM apps WHERE id = ?`, id)
}

func (a *AppRepo) GetByToken(ctx context.Context, token string) (store.App, error) {
	return getApp(ctx, a.db, `SELECT id, user_id, token, name FROM apps WHERE token = ?`, token)
}

func getApp(ctx context.Context, q queryExec, query string, args ...any) (store.App, error) {
	var a store.App
	if err := q.QueryRowContext(ctx, query, args...).Scan(&a.ID, &a.UserID, &a.Token, &a.Name); err != nil {
		return store.App{}, mapErr(err)
	}
	return a, nil
}

// DeviceRepo is the SQLite implementation of store.DeviceRepo.
type DeviceRepo struct{ db DB }

func (d *DeviceRepo) Create(ctx context.Context, in *store.Device) (int64, error) {
	res, err := d.db.ExecContext(ctx, `
INSERT INTO devices(user_id, device_id, secret_hash, name, model, os, fcm_token)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, in.DeviceID, in.SecretHash, in.Name, in.Model, in.OS, optStr(in.FCMToken))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("device last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

func (d *DeviceRepo) GetByDeviceID(ctx context.Context, deviceID string) (store.Device, error) {
	var (
		dv   store.Device
		fcm  sql.NullString
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT id, user_id, device_id, secret_hash, name, model, os, fcm_token FROM devices WHERE device_id = ?`,
		deviceID,
	).Scan(&dv.ID, &dv.UserID, &dv.DeviceID, &dv.SecretHash, &dv.Name, &dv.Model, &dv.OS, &fcm)
	if err != nil {
		return store.Device{}, mapErr(err)
	}
	dv.FCMToken = nullStr(fcm)
	return dv, nil
}
