package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pushfree/pushfree/internal/store"
)

// idAlphabet is the character set for device_id and secret values: 30-char
// [A-Za-z0-9], matching the pushfree identifier format.
const idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// idLength is the fixed length of every device_id and secret (30 chars).
const idLength = 30

// newSecret generates a cryptographically random idLength-char identifier.
// It uses crypto/rand.Int (math/big) per character so the distribution is
// unbiased (modulo on 62 would skew toward the first 8 symbols).
func newSecret() (string, error) {
	out := make([]byte, idLength)
	max := big.NewInt(int64(len(idAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = idAlphabet[n.Int64()]
	}
	return string(out), nil
}

// hashSecret returns the lower-case hex SHA-256 of secret, the form stored in
// devices.secret_hash. Only the hash is persisted; the plaintext is returned
// to the client exactly once at registration.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// authenticateDevice validates a (device_id, secret) pair against the stored
// hash. An unknown device, empty credential, or hash mismatch all yield
// ok=false; callers map that to a single auth-failure outcome (WS close 4001
// or HTTP 401) without distinguishing the cause, so it cannot be used to
// enumerate devices.
func authenticateDevice(ctx context.Context, devs store.DeviceRepo, deviceID, secret string) (store.Device, bool) {
	if deviceID == "" || secret == "" {
		return store.Device{}, false
	}
	dev, err := devs.GetByDeviceID(ctx, deviceID)
	if err != nil {
		return store.Device{}, false
	}
	if hashSecret(secret) != dev.SecretHash {
		return store.Device{}, false
	}
	return dev, true
}

// deviceNameRe is the allowed device-name character class.
var deviceNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validateDeviceName enforces name <= 25 chars (UTF-8 rune count, matching the
// schema CHECK(length(name)<=25)) over the [A-Za-z0-9_-] class. The handler
// validates before INSERT so a bad name yields a clean 400 rather than a raw
// constraint error.
func validateDeviceName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if utf8.RuneCountInString(name) > 25 {
		return errors.New("name must be 25 characters or fewer")
	}
	if !deviceNameRe.MatchString(name) {
		return errors.New("name may only contain letters, digits, underscore and hyphen")
	}
	return nil
}

// parseSince parses the since cursor (a message id). An absent/empty/negative
// value yields 0 (the latest page). It never returns an error: a malformed
// since is treated as 0 so a bad query cannot 500 the endpoint.
func parseSince(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
