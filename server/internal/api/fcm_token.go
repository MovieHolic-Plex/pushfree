package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// This file implements POST /1/devices/fcm_token.json, the FCM token
// registration endpoint. Android's FcmTokenRegistrar POSTs here with
// device_id + secret (device-secret auth, same as the ack endpoint) and
// the fcm_token to store. The token is later consumed by the FCM delivery
// channel (internal/fcm) when it is wired.
//
// Route registered alongside the other /1/* device/receipt routes in
// accounts.go Register.

// fcmTokenBody is the JSON shape accepted by POST /1/devices/fcm_token.json.
type fcmTokenBody struct {
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
	FCMToken string `json:"fcm_token"`
}

// fcmTokenHandler handles POST /1/devices/fcm_token.json.
//
// It authenticates the device via (device_id, secret), then stores the
// FCM registration token on the device row so the FCM delivery channel
// can push to it. A successful response is:
//
//	{"status":1,"request":"<uuid>"}
//
// An unknown device or wrong secret yields 401. An empty fcm_token yields 400.
func (a *Accounts) fcmTokenHandler(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()

	var body fcmTokenBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeRequestErrors(w, http.StatusBadRequest, requestID, "could not parse JSON body")
		return
	}

	if body.DeviceID == "" || body.Secret == "" {
		writeRequestErrors(w, http.StatusUnauthorized, requestID, "device_id and secret are required")
		return
	}
	if body.FCMToken == "" {
		writeRequestErrors(w, http.StatusBadRequest, requestID, "fcm_token is required")
		return
	}

	// Authenticate the device (mirrors authenticateAckDevice).
	dev, err := a.repos.Devices.GetByDeviceID(r.Context(), body.DeviceID)
	if err != nil {
		writeRequestErrors(w, http.StatusUnauthorized, requestID, "invalid device_id or secret")
		return
	}
	sum := sha256.Sum256([]byte(body.Secret))
	if hex.EncodeToString(sum[:]) != dev.SecretHash {
		writeRequestErrors(w, http.StatusUnauthorized, requestID, "invalid device_id or secret")
		return
	}

	// Store the FCM token on the device row.
	if err := a.repos.Devices.SetFCMToken(r.Context(), body.DeviceID, body.FCMToken); err != nil {
		a.logger.Error("fcm_token: store", "device", body.DeviceID, "err", err)
		writeRequestErrors(w, http.StatusInternalServerError, requestID, "could not store fcm token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": 1, "request": requestID})
}
