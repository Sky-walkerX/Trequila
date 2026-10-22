// Package crypto provides authenticated symmetric encryption (AES-256-GCM)
// for protecting user-supplied secrets (e.g. provider API keys) at rest.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Cipher encrypts and decrypts short secrets with AES-256-GCM.
type Cipher struct {
	gcm cipher.AEAD
}

// NewCipher builds a Cipher from a 32-byte AES-256 key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{gcm: gcm}, nil
}

// NewCipherFromBase64 decodes a standard-base64 32-byte key and builds a Cipher.
// This is the form expected for the ENCRYPTION_KEY environment variable.
func NewCipherFromBase64(encoded string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("crypto: invalid base64 key: %w", err)
	}
	return NewCipher(key)
}

// Encrypt returns base64( nonce || ciphertext+tag ).
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. It returns an error on a wrong key, truncation, or
// any tampering (GCM authentication failure).
func (c *Cipher) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("crypto: invalid base64 ciphertext: %w", err)
	}
	ns := c.gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("crypto: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plain, err := c.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: decryption failed: %w", err)
	}
	return string(plain), nil
}
