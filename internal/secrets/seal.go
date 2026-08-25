// Package secrets seals provider credentials before they reach storage.
//
// The key is supplied by the operator and never generated. Generating one on first boot
// would be friendlier and would produce installs that silently lose every linked mailbox on
// the next redeploy, so a missing key is a hard startup failure instead.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const KeyLen = 32

var (
	ErrKeyMissing = errors.New("no encryption key configured")
	ErrKeyLength  = fmt.Errorf("encryption key must be %d bytes when decoded", KeyLen)
	ErrCiphertext = errors.New("stored credential could not be decrypted; wrong key or corrupted data")
)

// Sealer encrypts and decrypts short secrets with AES-256-GCM. Each seal uses a fresh nonce,
// prepended to the ciphertext.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer accepts a base64 (standard or raw URL) encoded 32-byte key.
func NewSealer(encodedKey string) (*Sealer, error) {
	if encodedKey == "" {
		return nil, ErrKeyMissing
	}
	key, err := decodeKey(encodedKey)
	if err != nil {
		return nil, err
	}
	if len(key) != KeyLen {
		return nil, ErrKeyLength
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("encryption key is not valid base64")
}

// Seal encrypts plaintext. context is bound as additional authenticated data, so a sealed
// credential cannot be moved between accounts: decryption fails if the context differs.
func (s *Sealer) Seal(plaintext []byte, context string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plaintext, []byte(context))
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (s *Sealer) Open(encoded, context string) ([]byte, error) {
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrCiphertext
	}
	if len(raw) < s.aead.NonceSize() {
		return nil, ErrCiphertext
	}
	nonce, ct := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	out, err := s.aead.Open(nil, nonce, ct, []byte(context))
	if err != nil {
		return nil, ErrCiphertext
	}
	return out, nil
}

func (s *Sealer) SealString(plaintext, context string) (string, error) {
	return s.Seal([]byte(plaintext), context)
}

func (s *Sealer) OpenString(encoded, context string) (string, error) {
	b, err := s.Open(encoded, context)
	return string(b), err
}

// GenerateKey returns a fresh base64 key, for the message shown when none is configured.
func GenerateKey() (string, error) {
	key := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}
