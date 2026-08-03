// Package e2ee implements the Pushover-compatible end-to-end-encryption field
// format used by encrypted=1 messages (plan todo 43).
//
// Per-field wire format (each of message/title/url/url_title is encrypted
// INDEPENDENTLY with its own fresh 16-byte IV and HMAC):
//
//	GZIP(plaintext)
//	  -> AES-256-CBC (random 16-byte IV, PKCS7 padding)
//	  -> HMAC-SHA256(key, IV || ciphertext)
//	  -> base64( IV || ciphertext || hmac )
//
// The 256-bit key is carried as a 64-character hex string provisioned
// out-of-band (the server never receives it). The SAME key is reused for AES
// and HMAC, matching the upstream scheme verbatim; this package does not add
// key separation because its job is format compatibility, not a new design.
//
// Opaque handling: the server MUST NOT decrypt encrypted=1 traffic. It stores
// and transports the base64 field value untouched (see internal/api/messages.go,
// which assigns r.FormValue directly into the Send row). This package therefore
// exposes Decrypt only as the client-side reference implementation (todo 44
// wires it into the Android/desktop clients); the server never calls it on
// inbound data.
package e2ee

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// Format constants. They are exported so callers (tests, client ports) can
// reason about the byte layout without magic numbers.
const (
	// KeyLen is the AES-256 key length in bytes (256 bits).
	KeyLen = 32
	// KeyHexLen is the length of the hex-encoded key string (64 chars).
	KeyHexLen = 64
	// IVLen is the AES-CBC initialization vector length in bytes (16).
	IVLen = aes.BlockSize
	// HMACLen is the HMAC-SHA256 digest length in bytes (32).
	HMACLen = sha256.Size
	// minBlobLen is the smallest legal raw blob: IV + exactly one CBC block +
	// HMAC. Anything shorter cannot be a well-formed field.
	minBlobLen = IVLen + aes.BlockSize + HMACLen

	// gzipLevel is the fixed compression level. Combined with the zeroed
	// ModTime in gzipField, it makes encryptRaw deterministic for a given
	// (key, iv, plaintext), which the golden vector test relies on.
	gzipLevel = gzip.DefaultCompression
)

// Sentinel errors. Decrypt failures are collapsed into ErrInvalidBlob so a
// caller cannot tell a MAC mismatch from a padding/gzip failure -- this is
// padding-oracle hygiene for the legacy encrypt-then-MAC + CBC construction.
var (
	ErrInvalidKey  = errors.New("e2ee: key must be 64 hex chars (32 bytes)")
	ErrInvalidBlob = errors.New("e2ee: invalid or tampered encrypted blob")
	ErrShortBlob   = errors.New("e2ee: encrypted blob is too short")
)

// ParseKey decodes the 64-character hex form of an E2EE key into its 32 raw
// bytes. Anything that is not exactly 64 hex characters is rejected.
func ParseKey(hexKey string) ([]byte, error) {
	if len(hexKey) != KeyHexLen {
		return nil, ErrInvalidKey
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, ErrInvalidKey
	}
	if len(key) != KeyLen {
		return nil, ErrInvalidKey
	}
	return key, nil
}

// Encrypt encrypts plaintext under the A1 field scheme using a fresh random
// 16-byte IV read from crypto/rand, and returns the base64 blob. key must be
// 32 bytes (use ParseKey to obtain it from the 64-hex form).
//
// The server MUST NOT call this on inbound traffic; it is the sender/client
// reference path (the server stores encrypted=1 fields opaquely).
func Encrypt(key, plaintext []byte) (string, error) {
	if len(key) != KeyLen {
		return "", ErrInvalidKey
	}
	iv := make([]byte, IVLen)
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("e2ee: generate IV: %w", err)
	}
	raw, err := encryptRaw(key, iv, plaintext)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// EncryptField is the string-in/string-out convenience: it parses the 64-hex
// key and returns the base64 blob for the plaintext field.
func EncryptField(hexKey, plaintext string) (string, error) {
	key, err := ParseKey(hexKey)
	if err != nil {
		return "", err
	}
	return Encrypt(key, []byte(plaintext))
}

// Decrypt reverses Encrypt: base64-decode, split the trailing HMAC, verify it
// in constant time (encrypt-then-MAC -- the MAC is checked BEFORE any CBC or
// PKCS7 work), then CBC-decrypt, PKCS7-unpad, and gunzip.
//
// All structural failures return ErrInvalidBlob or ErrShortBlob with no detail
// distinguishing a MAC failure from a padding/gzip failure, to avoid leaking a
// padding oracle on the legacy CBC construction. key must be 32 bytes.
func Decrypt(key []byte, blob string) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, ErrInvalidKey
	}
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return nil, ErrInvalidBlob
	}
	// Layout: IV (16) || ct (non-zero, multiple of BlockSize) || hmac (32).
	if len(raw) < minBlobLen {
		return nil, ErrShortBlob
	}
	ctLen := len(raw) - IVLen - HMACLen
	if ctLen <= 0 || ctLen%aes.BlockSize != 0 {
		return nil, ErrShortBlob
	}
	macOff := len(raw) - HMACLen
	iv := raw[:IVLen]
	ct := raw[IVLen:macOff]
	mac := raw[macOff:]

	// Verify-then-decrypt. hmac.Equal is constant-time.
	want := hmacSHA256(key, raw[:macOff])
	if !hmac.Equal(want, mac) {
		return nil, ErrInvalidBlob
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		// Unreachable for a 32-byte key, but keep the guard.
		return nil, ErrInvalidBlob
	}
	padded := make([]byte, ctLen)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(padded, ct)

	// PKCS7 unpad with full validation: the pad length must be in [1, BlockSize]
	// and every pad byte must equal it. Any anomaly is reported as a generic
	// invalid blob (never a distinct padding error).
	padLen := int(padded[len(padded)-1])
	if padLen == 0 || padLen > aes.BlockSize {
		return nil, ErrInvalidBlob
	}
	for i := len(padded) - padLen; i < len(padded); i++ {
		if padded[i] != byte(padLen) {
			return nil, ErrInvalidBlob
		}
	}
	compressed := padded[:len(padded)-padLen]

	pt, err := gunzipField(compressed)
	if err != nil {
		return nil, ErrInvalidBlob
	}
	return pt, nil
}

// DecryptField is the string-in/string-out convenience.
func DecryptField(hexKey, blob string) (string, error) {
	key, err := ParseKey(hexKey)
	if err != nil {
		return "", err
	}
	pt, err := Decrypt(key, blob)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// encryptRaw is the deterministic core (no randomness): it encrypts plaintext
// under the already-validated key+iv and returns the raw IV||ct||hmac bytes.
// Production uses Encrypt (which supplies a fresh random iv); this is kept
// unexported and exercised by the in-package golden-vector test.
func encryptRaw(key, iv, plaintext []byte) ([]byte, error) {
	compressed, err := gzipField(plaintext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	// PKCS7 pad: always adds 1..BlockSize bytes, so an already-aligned input
	// still grows by a full block.
	padLen := aes.BlockSize - len(compressed)%aes.BlockSize
	padded := make([]byte, len(compressed)+padLen)
	copy(padded, compressed)
	for i := len(compressed); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	// blob = IV || ct, then append HMAC over (IV || ct).
	blob := make([]byte, 0, IVLen+len(ct)+HMACLen)
	blob = append(blob, iv...)
	blob = append(blob, ct...)
	blob = append(blob, hmacSHA256(key, blob)...)
	return blob, nil
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// gzipField gzip-compresses plaintext. The ModTime is forced to the zero value
// so the gzip header carries no wall-clock (it would otherwise leak sender
// timing into the ciphertext) and so the output is byte-stable for the golden
// vector; the compression level is fixed via gzipLevel for the same reason.
func gzipField(plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzipLevel)
	if err != nil {
		return nil, err
	}
	gw.Header.ModTime = time.Time{}
	if _, err := gw.Write(plaintext); err != nil {
		_ = gw.Close()
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gunzipField reverses gzipField.
func gunzipField(compressed []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	pt, err := io.ReadAll(gr)
	if err != nil {
		return nil, err
	}
	return pt, nil
}
