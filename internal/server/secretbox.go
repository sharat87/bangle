package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Upstream credentials (access and refresh tokens, client secrets) cannot be
// hashed: Bangle has to present them. They are encrypted at rest with
// AES-256-GCM under SECRETS_KEY. A reader of the database or of a backup gets ciphertext; whoever holds the
// environment holds the key, so this guards the database file, not a
// compromised process. Losing the key loses every login: each server has to
// be connected again.
//
// A sealed value is "v1:" + base64(nonce || ciphertext). Every value in a
// sealed column is sealed, the empty string included, and open refuses one
// that is not rather than read it as plaintext.

const sealedPrefix = "v1:"

// SecretBox seals and opens stored credentials.
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox builds the box from SECRETS_KEY's value: 32 bytes, base64.
// cmd/server calls it at boot, so a missing or malformed key stops the server
// instead of failing at the first login.
func NewSecretBox(encodedKey string) (*SecretBox, error) {
	if encodedKey == "" {
		return nil, errors.New("SECRETS_KEY environment variable is required (openssl rand -base64 32)")
	}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("SECRETS_KEY must be 32 random bytes, base64-encoded (openssl rand -base64 32)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

// seal encrypts plain for storage, under a fresh nonce.
func (b *SecretBox) seal(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return sealedPrefix + base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// open decrypts a value seal produced. Anything else (a plain value, or one
// sealed under another key or tampered with) is an error.
func (b *SecretBox) open(sealed string) (string, error) {
	rest, ok := strings.CutPrefix(sealed, sealedPrefix)
	if !ok {
		return "", errors.New("stored credential is not encrypted")
	}
	data, err := base64.StdEncoding.DecodeString(rest)
	n := b.aead.NonceSize()
	if err != nil || len(data) < n {
		return "", errors.New("stored credential is malformed")
	}
	plain, err := b.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return "", fmt.Errorf("stored credential does not decrypt (was SECRETS_KEY changed?)")
	}
	return string(plain), nil
}
