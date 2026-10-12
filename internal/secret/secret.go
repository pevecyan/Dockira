// Package secret encrypts values stored in SQLite (notification channel tokens and the like)
// with AES-256-GCM, using a key derived from the config's secret_key.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// version is the first byte of every sealed value, so the format (cipher, key derivation) can
// change later without guessing what old rows contain.
const version byte = 1

const nonceSize = 12

// ErrOpen is returned for anything that fails to decrypt: wrong key, tampering, truncation.
// It deliberately doesn't say which, there's nothing useful to learn from the difference.
var ErrOpen = errors.New("secret: cannot decrypt (wrong key or corrupted data)")

// Box seals and opens values for one purpose. It is safe for concurrent use.
type Box struct {
	aead cipher.AEAD
}

// NewBox derives a 32-byte key from masterKey with HKDF-SHA256, using purpose as the info
// string. Different purposes get unrelated keys, so a value sealed for one use can't be opened
// (or swapped in) as another.
func NewBox(masterKey []byte, purpose string) (*Box, error) {
	if len(masterKey) < 32 {
		return nil, fmt.Errorf("secret: master key has %d bytes, needs at least 32", len(masterKey))
	}
	if purpose == "" {
		return nil, errors.New("secret: purpose is empty")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, purpose, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal returns version || nonce || ciphertext+tag. The nonce is random: 96 bits is plenty for
// the handful of values this app ever encrypts under one key.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	out := make([]byte, 1+nonceSize, 1+nonceSize+len(plaintext)+b.aead.Overhead())
	out[0] = version
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, err
	}
	// The version byte is authenticated too, so it can't be flipped without detection.
	return b.aead.Seal(out, out[1:], plaintext, out[:1]), nil
}

// Open reverses Seal. Any modification of the sealed value, or the wrong key, yields ErrOpen.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	if len(sealed) < 1+nonceSize+b.aead.Overhead() {
		return nil, ErrOpen
	}
	if sealed[0] != version {
		return nil, fmt.Errorf("secret: unknown format version %d", sealed[0])
	}
	pt, err := b.aead.Open(nil, sealed[1:1+nonceSize], sealed[1+nonceSize:], sealed[:1])
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}
