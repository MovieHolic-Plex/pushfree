// Package api implements the pushfree account-management HTTP surface:
// open registration with admin bootstrap, session login, identity, and
// quiet-hours settings. It is intentionally distinct from the
// Pushover-compatible ingest API (/1/...); these routes live under /v1/ and
// use JSON, not form-encoding.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters per RFC 9106 (memory-hard). m=64 MiB expressed in KiB.
const (
	argon2Time    = uint32(3)
	argon2Memory  = uint32(64 * 1024) // KiB == 64 MiB
	argon2Paral   = uint8(4)
	argon2KeyLen  = uint32(32)
	argon2SaltLen = 16
)

var (
	errBadHashFormat = errors.New("api: invalid password hash format")
	emailRe          = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	hhmmRe           = regexp.MustCompile(`^\d{2}:\d{2}$`)
	userKeyRe        = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)
)

// keyAlphabet is the [A-Za-z0-9] space from which user_key (and later token)
// identifiers are drawn. newUserKey rejects bytes that would introduce modulo
// bias against the 62-symbol alphabet.
const keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newAppToken returns a 30-char [A-Za-z0-9] app token from crypto/rand. It
// shares the alphabet, length, and unbiased rejection-sampled generator with
// user_key; the two identifier kinds are format-identical by spec
// (apps.token has the same CHECK(length(token)=30) constraint).
func newAppToken() (string, error) { return newUserKey() }

// newUserKey returns a 30-char [A-Za-z0-9] identifier sourced from crypto/rand
// with rejection sampling so the distribution is unbiased.
func newUserKey() (string, error) {
	const n = 30
	const slots = len(keyAlphabet) // 62
	// Keep only byte values that map evenly onto the alphabet (0..k*slots-1,
	// k=256/slots=4 -> 0..247); reject the trailing biased tail (248..255).
	const limit = (256 / slots) * slots
	buf := make([]byte, n)
	out := make([]byte, 0, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("api: read random: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, keyAlphabet[int(b)%slots])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// hashPassword returns a self-describing PHC-format argon2id string carrying
// the salt and parameters, so verifyPassword needs no shared configuration.
func hashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("api: read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Paral, argon2KeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argon2Memory, argon2Time, argon2Paral,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

// verifyPassword parses a PHC-format argon2id string and checks the password
// against it in constant time. An unparseable hash is reported via err so the
// caller can treat it as a server fault rather than a wrong password.
func verifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHashFormat
	}
	var m, t uint32
	var p uint8
	for _, field := range strings.Split(parts[3], ",") {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			return false, errBadHashFormat
		}
		switch kv[0] {
		case "m":
			v, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil {
				return false, errBadHashFormat
			}
			m = uint32(v)
		case "t":
			v, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil {
				return false, errBadHashFormat
			}
			t = uint32(v)
		case "p":
			v, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil {
				return false, errBadHashFormat
			}
			p = uint8(v)
		default:
			return false, errBadHashFormat
		}
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errBadHashFormat
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, errBadHashFormat
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// --- Stateless signed-cookie sessions ---------------------------------------

// A session value is "<userID>:<expiryUnix>" + "." + base64url(HMAC-SHA256).
// The schema is frozen (no sessions table), so authenticity and expiry are
// enforced entirely by the signature; tampering with either field invalidates
// the MAC. The server auth secret is the only shared state.

const sessionCookieName = "pushfree_session"

// sessionTTL is how long a login cookie remains valid. Sessions are stateless,
// so there is no server-side revocation list; rotating auth-secret invalidates
// all outstanding sessions at once.
const sessionTTL = 30 * 24 * time.Hour

type userIDCtxKey struct{}

func getUserID(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(userIDCtxKey{}).(int64)
	return v, ok
}

func signSession(secret []byte, userID int64, expiry int64) string {
	payload := strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// parseSession validates the signature and expiry and returns the userID.
func parseSession(secret []byte, cookie string) (int64, bool) {
	payload, sig, ok := strings.Cut(cookie, ".")
	if !ok {
		return 0, false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	wantSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(wantSig)) {
		return 0, false
	}
	uidStr, expStr, ok := strings.Cut(payload, ":")
	if !ok {
		return 0, false
	}
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil {
		return 0, false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return 0, false
	}
	if time.Now().Unix() >= exp {
		return 0, false
	}
	return uid, true
}

// --- Validation -------------------------------------------------------------

func validEmail(s string) bool { return emailRe.MatchString(s) }

// validHHMM checks "HH:MM" with 00<=HH<=23 and 00<=MM<=59.
func validHHMM(s string) bool {
	if !hhmmRe.MatchString(s) {
		return false
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h <= 23 && m <= 59
}

// requireSession is the auth middleware for session-protected routes. An
// absent, tampered, or expired cookie yields 401.
func (a *Accounts) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeErrors(w, http.StatusUnauthorized, "session required")
			return
		}
		uid, ok := parseSession(a.authSecret, c.Value)
		if !ok {
			writeErrors(w, http.StatusUnauthorized, "session invalid or expired")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), userIDCtxKey{}, uid))
		next(w, r)
	}
}
