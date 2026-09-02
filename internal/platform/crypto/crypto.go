// Package crypto provides AES-GCM at-rest encryption for secrets such as
// per-tenant OAuth client secrets (ADR-7).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrCiphertextTooShort indicates malformed ciphertext.
var ErrCiphertextTooShort = errors.New("crypto: ciphertext too short")

// AESGCM encrypts/decrypts with a 32-byte key derived from the configured
// base64 envelope key (APP_ENCRYPTION_KEY). Uses AES-256-GCM with random
// nonces, prepended to ciphertext.
type AESGCM struct {
	aead cipher.AEAD
}

// NewAESGCM builds an AESGCM from a base64-encoded key. Accepts keys of any
// length >= 16 bytes (hashed to 32 bytes via SHA-256 for stable sizing).
func NewAESGCM(base64Key string) (*AESGCM, error) {
	raw, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode APP_ENCRYPTION_KEY: %w", err)
	}
	if len(raw) < 16 {
		return nil, errors.New("crypto: APP_ENCRYPTION_KEY must be at least 16 bytes when base64-decoded")
	}
	sum := sha256.Sum256(raw)
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	return &AESGCM{aead: aead}, nil
}

// Encrypt seals plaintext and returns base64(nonce || ciphertext).
func (a *AESGCM) Encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: nonce: %w", err)
	}
	sealed := a.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens base64(nonce || ciphertext) produced by Encrypt.
func (a *AESGCM) Decrypt(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode: %w", err)
	}
	ns := a.aead.NonceSize()
	if len(raw) < ns {
		return nil, ErrCiphertextTooShort
	}
	plain, err := a.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: open: %w", err)
	}
	return plain, nil
}
