package sqlite

import (
	"context"
	"database/sql"

	"github.com/pushfree/pushfree/internal/store"
)

// ListByUser returns every device owned by userID in ascending id
// (registration) order, with fcm_token resolved to "" when NULL. It backs the
// devices[] field of POST /1/users/validate.json (todo 11). The method lives
// in its own file so the validate endpoint can read device names without
// touching app_device.go, which is worker 13's device-registration territory;
// only a read path is added here, no new writes.
func (d *DeviceRepo) ListByUser(ctx context.Context, userID int64) ([]store.Device, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, user_id, device_id, secret_hash, name, model, os, fcm_token FROM devices WHERE user_id = ? ORDER BY id ASC`,
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
