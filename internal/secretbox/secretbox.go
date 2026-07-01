// Package secretbox provides authenticated at-rest encryption (AES-256-GCM) for
// secret columns (account access/refresh tokens, session cookies). Values are stored
// as a self-describing string: an "enc:v1:" prefix followed by base64(nonce||ciphertext).
//
// Two invariants make adoption safe and reversible:
//   - Open() returns any value WITHOUT the prefix unchanged, so legacy plaintext rows
//     keep working and migration can be lazy.
//   - Seal()/Open() with a nil/short key (encryption disabled) pass plaintext through,
//     so tests and in-memory stores that never set a key behave exactly as before.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

// Prefix marks an encrypted value. Versioned so the scheme can evolve.
const Prefix = "enc:v1:"

// DeriveKey turns an arbitrary-length secret into a 32-byte AES-256 key.
func DeriveKey(secret []byte) []byte {
	sum := sha256.Sum256(secret)
	return sum[:]
}

// Seal encrypts plaintext and returns Prefix+base64(nonce||ciphertext). Empty plaintext
// is returned unchanged (nothing to protect). A key that is not 32 bytes means
// "encryption disabled" and plaintext is returned unchanged.
func Seal(key []byte, plaintext string) (string, error) {
	if plaintext == "" || len(key) != 32 {
		return plaintext, nil
	}
	gcm, err := newGCM(key)
	if err != nil {
		return plaintext, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return plaintext, err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Open reverses Seal. A value without Prefix is returned unchanged (legacy plaintext
// passthrough). A prefixed value requires a valid key and authenticates the ciphertext;
// a wrong key (e.g. the operator rotated identity_secret) yields an error.
func Open(key []byte, s string) (string, error) {
	if !strings.HasPrefix(s, Prefix) {
		return s, nil
	}
	if len(key) != 32 {
		return "", errors.New("secretbox: value is encrypted but no key is configured")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("secretbox: ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// IsSealed reports whether a stored value is already encrypted.
func IsSealed(s string) bool { return strings.HasPrefix(s, Prefix) }

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
