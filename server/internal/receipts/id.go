package receipts

import (
	"crypto/rand"
	"errors"
)

const (
	// idLen is the exact length of a receipt id, matching the schema's
	// receipts.id CHECK(length(id)=30) constraint and the Pushover receipt-id
	// format.
	idLen = 30
	// idAlphabet is the [A-Za-z0-9] alphabet a receipt id may use.
	idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// ErrIDEntropy is returned if the crypto/rand source fails while generating
// an id. It must not be swallowed: a receipt id is required to persist the
// priority-2 lifecycle row.
var ErrIDEntropy = errors.New("receipts: failed to read random bytes for id")

// NewID returns a freshly generated 30-char receipt id over [A-Za-z0-9].
//
// It uses crypto/rand and rejection sampling so every one of the 62 alphabet
// symbols is selected uniformly (modulo bias is avoided rather than tolerated),
// keeping ids unguessable. The receipts subsystem is the flight-software part
// of the project, so the small extra cost of rejection sampling is warranted.
func NewID() (string, error) {
	// max is the largest multiple of len(idAlphabet) <= 256. Bytes >= max are
	// rejected and re-read, eliminating modulo bias. 62*4 == 248, so max=248.
	const max = 256 - (256 % len(idAlphabet))
	out := make([]byte, idLen)
	for i := 0; i < idLen; {
		var buf [1]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", ErrIDEntropy
		}
		if int(buf[0]) >= max {
			continue // reject to keep the distribution uniform
		}
		out[i] = idAlphabet[int(buf[0])%len(idAlphabet)]
		i++
	}
	return string(out), nil
}
