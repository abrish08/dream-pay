// Package secrets provides authenticated encryption for secrets the server
// must read back later (webhook signing secrets, provider credentials) and
// HMAC helpers. Values that never need to be read back (API keys) are hashed,
// not encrypted.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

const version = "v1"

// Box encrypts with AES-256-GCM. The key comes from a secret manager
// (e.g. Azure Key Vault), never from the database.
type Box struct{ aead cipher.AEAD }

func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must be 32 bytes")
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

// Encrypt returns "v1:<base64(nonce|ciphertext|tag)>". The version prefix
// allows key rotation later.
func (b *Box) Encrypt(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return version + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

func (b *Box) Decrypt(stored string) (string, error) {
	v, data, ok := strings.Cut(stored, ":")
	if !ok || v != version {
		return "", errors.New("unknown ciphertext version")
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func HMACHex(key, msg []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

func EqualHex(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// RandomToken returns n random bytes, base64url encoded.
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
