package e2ee

import (
	"encoding/hex"
	"testing"
)

// Temporary probe to capture the deterministic golden vector. Replaced by the
// real assertion test once the bytes are printed.
func TestProbeVector(t *testing.T) {
	hexKey := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	ivHex := "101112131415161718191a1b1c1d1e1f"
	plaintext := "Pushfree E2EE test vector"
	key, _ := hex.DecodeString(hexKey)
	iv, _ := hex.DecodeString(ivHex)
	raw, err := encryptRaw(key, iv, []byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("len=%d", len(raw))
	t.Logf("iv   = %x", raw[:16])
	t.Logf("ct   = %x", raw[16:len(raw)-32])
	t.Logf("hmac = %x", raw[len(raw)-32:])
	t.Logf("raw  = %x", raw)
}
