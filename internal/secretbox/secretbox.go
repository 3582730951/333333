// Package secretbox provides versioned authenticated at-rest encryption for relay
// secrets. Version 2 records a key id and derives a distinct AES-256-GCM key for
// every domain with HKDF-SHA256. Version 1 remains readable only for migration.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const (
	// Prefix is the only format written by current code.
	Prefix        = "enc:v2:"
	LegacyPrefix  = "enc:v1:"
	DefaultDomain = "storage"
)

// DeriveKey turns an arbitrary-length secret into a 32-byte AES-256 master key.
func DeriveKey(secret []byte) []byte {
	sum := sha256.Sum256(secret)
	return sum[:]
}

// KeyID is a non-secret, stable identifier embedded in version 2 ciphertext.
func KeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// Seal encrypts a value in the default storage domain.
func Seal(key []byte, plaintext string) (string, error) {
	return SealDomain(key, DefaultDomain, plaintext)
}

// SealDomain encrypts plaintext and binds it to domain. An invalid key is an
// error rather than a plaintext fallback.
func SealDomain(key []byte, domain, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(key) != 32 {
		return "", errors.New("secretbox: a 32-byte key is required")
	}
	domain = normalizeDomain(domain)
	derived, err := deriveDomainKey(key, domain)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(derived)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	id := KeyID(key)
	aad := []byte(Prefix + id + ":" + domain)
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), aad)
	return Prefix + id + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

// Open decrypts a value in the default storage domain.
func Open(key []byte, value string) (string, error) {
	return OpenDomain(key, DefaultDomain, value)
}

// OpenDomain reads version 2 values and legacy version 1 ciphertext. Plaintext
// passthrough is retained solely so callers can run an explicit migration before
// enabling strict mode.
func OpenDomain(key []byte, domain, value string) (string, error) {
	switch {
	case strings.HasPrefix(value, Prefix):
		return openV2(key, normalizeDomain(domain), value)
	case strings.HasPrefix(value, LegacyPrefix):
		return openV1(key, value)
	default:
		return value, nil
	}
}

// OpenDomainWithKeys supports a bounded rotation window. It never returns an
// encrypted value as plaintext.
func OpenDomainWithKeys(keys [][]byte, domain, value string) (string, error) {
	if !IsSealed(value) {
		return value, nil
	}
	var last error
	for _, key := range keys {
		plain, err := OpenDomain(key, domain, value)
		if err == nil {
			return plain, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("secretbox: no decryption key configured")
	}
	return "", last
}

func openV2(key []byte, domain, value string) (string, error) {
	if len(key) != 32 {
		return "", errors.New("secretbox: value is encrypted but no key is configured")
	}
	rest := strings.TrimPrefix(value, Prefix)
	id, encoded, ok := strings.Cut(rest, ":")
	if !ok || id == "" || encoded == "" {
		return "", errors.New("secretbox: malformed version 2 value")
	}
	if id != KeyID(key) {
		return "", fmt.Errorf("secretbox: key id %s is unavailable", id)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	derived, err := deriveDomainKey(key, domain)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(derived)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("secretbox: ciphertext too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	aad := []byte(Prefix + id + ":" + domain)
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func openV1(key []byte, value string) (string, error) {
	if len(key) != 32 {
		return "", errors.New("secretbox: legacy value is encrypted but no key is configured")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, LegacyPrefix))
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("secretbox: legacy ciphertext too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// IsSealed reports whether a stored value is encrypted in a known format.
func IsSealed(value string) bool {
	return strings.HasPrefix(value, Prefix) || strings.HasPrefix(value, LegacyPrefix)
}

// IsCurrent reports whether a value uses the current version and key.
func IsCurrent(key []byte, value string) bool {
	return len(key) == 32 && strings.HasPrefix(value, Prefix+KeyID(key)+":")
}

func normalizeDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return DefaultDomain
	}
	return domain
}

func deriveDomainKey(master []byte, domain string) ([]byte, error) {
	reader := hkdf.New(sha256.New, master, nil, []byte("codex-pool/secretbox/v2/"+domain))
	key := make([]byte, 32)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, err
	}
	return key, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
