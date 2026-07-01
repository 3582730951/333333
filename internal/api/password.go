package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"hash"
	"strconv"
	"strings"
)

// password.go implements end-user password hashing with PBKDF2-HMAC-SHA256 using
// only the Go standard library, so the project keeps its minimal dependency set
// (no golang.org/x/crypto). The stored verifier is self-describing:
//
//	pbkdf2$<iterations>$<salt_b64>$<hash_b64>
//
// so the iteration count can be raised later without invalidating old hashes.

const (
	pbkdf2Iterations = 210000 // OWASP-recommended floor for PBKDF2-HMAC-SHA256
	pbkdf2KeyLen     = 32
	pbkdf2SaltLen    = 16
)

// hashPassword returns a new PBKDF2 verifier string for the given plaintext.
func hashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2Key([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// verifyPassword reports whether plaintext matches the stored verifier. The
// comparison is constant-time. A malformed/empty verifier never matches.
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > 10_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2Key([]byte(password), salt, iter, len(want), sha256.New)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2Key is RFC 8018 PBKDF2 (ported from golang.org/x/crypto/pbkdf2, BSD) so we
// stay stdlib-only.
func pbkdf2Key(password, salt []byte, iter, keyLen int, h func() hash.Hash) []byte {
	prf := hmac.New(h, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:4])
		dk = prf.Sum(dk)
		T := dk[len(dk)-hashLen:]
		copy(U, T)

		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(U)
			U = prf.Sum(U[:0])
			for x := range U {
				T[x] ^= U[x]
			}
		}
	}
	return dk[:keyLen]
}
