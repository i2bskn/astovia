// Package secretbox seals values that are stored in the database but must be
// read back, such as GitHub tokens and the VAPID private key.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// ErrUndecryptable is returned for a sealed value that is malformed, was
// tampered with, was sealed for another purpose or with another server
// secret. The cause is not distinguished.
var ErrUndecryptable = errors.New("sealed value cannot be decrypted")

const prefix = "v1."

// Box seals and opens values with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// New derives the sealing key from the server secret (ASTOVIA_JWT_SECRET),
// so that the JWT signing key is not used directly. Changing the secret makes
// every sealed value undecryptable.
func New(secret []byte) *Box {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("astovia secretbox key v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		// A 32-byte key is always a valid AES-256 key.
		panic("secretbox: " + err.Error())
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic("secretbox: " + err.Error())
	}
	return &Box{aead: aead}
}

// Seal encrypts plaintext with a random nonce. purpose is authenticated as
// additional data: it names the use and the owner of the value (for example
// "github-token:" + user ID), so a value copied to another row cannot be
// opened there.
func (b *Box) Seal(purpose, plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(purpose))
	return prefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decrypts a value from Seal with the same purpose.
func (b *Box) Open(purpose, sealed string) (string, error) {
	raw, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return "", ErrUndecryptable
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) < b.aead.NonceSize()+b.aead.Overhead() {
		return "", ErrUndecryptable
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, data[:n], data[n:], []byte(purpose))
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(plain), nil
}
