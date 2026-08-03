package e2ee

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// This file was originally a throwaway probe that printed the deterministic
// golden vector. It is now the Go consumer of the SHARED fixture
// `testdata/e2ee_vectors.json` — the single source of truth for E2EE test
// vectors, also consumed by the Android (`*E2ee*`) and desktop (`cargo test
// e2ee`) client suites (plan todo 44). Every platform must pass the SAME
// vectors; this test guards that the fixture itself is internally consistent
// with this reference Go implementation.

// vectorCase mirrors one entry in the shared JSON fixture.
type vectorCase struct {
	Name        string `json:"name"`
	KeyHex      string `json:"key_hex"`
	IVHex       string `json:"iv_hex"`
	Plaintext   string `json:"plaintext"`
	Blob        string `json:"blob"`
	ExpectError bool   `json:"expect_error,omitempty"`
	Note        string `json:"note,omitempty"`
}

type vectorFile struct {
	Format    string       `json:"format"`
	KeyHexLen int          `json:"key_hex_len"`
	IVLen     int          `json:"iv_len"`
	HMACLen   int          `json:"hmac_len"`
	Vectors   []vectorCase `json:"vectors"`
}

// loadVectors reads the shared fixture from testdata/. The path is resolved
// relative to this package so the test is independent of the working directory
// it is invoked from.
func loadVectors(t *testing.T) []vectorCase {
	t.Helper()
	path := filepath.Join("testdata", "e2ee_vectors.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared fixture %s: %v", path, err)
	}
	var f vectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse shared fixture %s: %v", path, err)
	}
	if len(f.Vectors) == 0 {
		t.Fatalf("shared fixture %s contains no vectors", path)
	}
	// Sanity-check the declared layout constants match the implementation.
	if f.KeyHexLen != KeyHexLen || f.IVLen != IVLen || f.HMACLen != HMACLen {
		t.Fatalf("fixture layout constants (%d,%d,%d) differ from impl (%d,%d,%d)",
			f.KeyHexLen, f.IVLen, f.HMACLen, KeyHexLen, IVLen, HMACLen)
	}
	return f.Vectors
}

// TestSharedVectors is the Go golden-vector assertion. It reads the JSON
// fixture shared with the clients and asserts that Decrypt reproduces every
// positive plaintext and rejects every negative (tampered / wrong-key) blob.
// The fixture is also the file printed by the manual QA step.
func TestSharedVectors(t *testing.T) {
	for _, c := range loadVectors(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			pt, err := DecryptField(c.KeyHex, c.Blob)
			if c.ExpectError {
				if err == nil {
					t.Fatalf("%s: expected decryption error, got plaintext %q", c.Name, pt)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: DecryptField failed: %v", c.Name, err)
			}
			if pt != c.Plaintext {
				t.Errorf("%s: plaintext mismatch\nhave %q\nwant %q", c.Name, pt, c.Plaintext)
			}
		})
	}
}
