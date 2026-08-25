package secrets

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
)

// Derive produces a subkey for some use other than sealing a credential.
//
// The operator supplies one secret, and that is worth keeping: a second key to generate, set
// and back up is a second key to lose. But the same 32 bytes driving AES-GCM in one place and
// an HMAC in another is one secret with two meanings, and "what can somebody holding
// MAILROOM_ENCRYPTION_KEY do" stops having a single answer. HKDF costs a hash and gives every
// consumer a key that exists only for its own purpose, so a signing key can never be a
// sealing key by accident and rotating the root still rotates both.
//
// The purpose string is the separation. Two callers passing the same one share a key, which
// is why it names a version as well as a use: changing the string is how a signing key is
// retired, and doing so invalidates every signature made under the old one.
func Derive(encodedKey, purpose string, length int) ([]byte, error) {
	if encodedKey == "" {
		return nil, ErrKeyMissing
	}
	if purpose == "" {
		return nil, errors.New("a derived key needs a purpose; without one it is the same key as every other derivation")
	}
	key, err := decodeKey(encodedKey)
	if err != nil {
		return nil, err
	}
	if len(key) != KeyLen {
		return nil, ErrKeyLength
	}
	return hkdf.Key(sha256.New, key, nil, purpose, length)
}
