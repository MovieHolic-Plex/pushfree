// Package sqlite fcm.go holds the FCM-channel store additions (todo 16).
// It is deliberately a separate, append-only file: it adds the single method
// the FCM delivery channel needs on DeviceRepo without touching the existing
// app_device.go registration surface.
package sqlite

import "context"

// ClearFCMToken nulls fcm_token for the device matching deviceID. Used by the
// FCM delivery channel (internal/fcm, todo 16) when FCM reports the token is
// UNREGISTERED or INVALID_ARGUMENT: the token is dead, so the device must
// re-register before it can receive FCM again.
//
// It is idempotent: a missing device or a device with no token updates zero
// rows and returns nil, because the caller already knows the send failed and
// only needs the token cleared if it is present.
func (d *DeviceRepo) ClearFCMToken(ctx context.Context, deviceID string) error {
	if _, err := d.db.ExecContext(ctx,
		`UPDATE devices SET fcm_token = NULL WHERE device_id = ?`, deviceID); err != nil {
		return mapErr(err)
	}
	return nil
}

// SetFCMToken stores the FCM registration token on the device matching
// deviceID, overwriting any existing token. Called by POST
// /1/devices/fcm_token.json when a client registers or refreshes its token.
// An unknown device_id updates zero rows and returns nil (the caller has
// already authenticated the device, so this should not happen in practice).
func (d *DeviceRepo) SetFCMToken(ctx context.Context, deviceID, fcmToken string) error {
	if _, err := d.db.ExecContext(ctx,
		`UPDATE devices SET fcm_token = ? WHERE device_id = ?`, fcmToken, deviceID); err != nil {
		return mapErr(err)
	}
	return nil
}
