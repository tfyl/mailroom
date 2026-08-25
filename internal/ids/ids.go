// Package ids generates the identifiers used across mailroom.
//
// Identifiers are prefixed and time-ordered so that they sort chronologically and are
// recognisable in logs and audit rows. They are also permanent: grants store account ids, and
// an account id must never be reused.
package ids

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"time"
)

var encoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// An identifier is six bytes of millisecond timestamp followed by ten of randomness.
//
// Six bytes hold milliseconds until the year 10889, and putting them first is what makes the
// encoded form sort chronologically. The randomness is what makes it unguessable, and ten
// bytes of it is the part that matters: these are not only database keys. The same generator
// produces the OAuth `state` for a mailbox link and the id of a pending authorization, where
// a value an attacker can predict is a way into somebody's account.
//
// An earlier version wrote the timestamp into eight bytes of a ten-byte buffer and then read
// randomness into the last four — which left 32 bits of entropy, not the eighty its length
// suggested, and overwrote two timestamp bytes so that ordering was random inside any
// 65-second window. Two identifiers collided after 67,196 draws.
const (
	timestampBytes = 6
	randomBytes    = 10
)

// New returns a prefixed, time-ordered identifier such as "acct_01JB4X8Q2K7M3FGHJK0NPQ".
func New(prefix string) string {
	var buf [timestampBytes + randomBytes]byte

	ms := uint64(time.Now().UnixMilli())
	for i := range timestampBytes {
		buf[timestampBytes-1-i] = byte(ms >> (8 * i))
	}
	if _, err := rand.Read(buf[timestampBytes:]); err != nil {
		// crypto/rand failing is not a condition worth degrading through: an identifier
		// without entropy would be guessable.
		panic(fmt.Sprintf("ids: reading random bytes: %v", err))
	}
	return prefix + "_" + encoding.EncodeToString(buf[:])
}

func Account() string { return New("acct") }
func Grant() string   { return New("grant") }
func Client() string  { return New("client") }

// Token returns an opaque high-entropy secret for bearer tokens and authorization codes.
func Token() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return encoding.EncodeToString(b), nil
}
