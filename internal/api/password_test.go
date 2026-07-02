package api

import "testing"

func TestPasswordHashAndVerify(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := hashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(pw, h) {
		t.Fatalf("correct password did not verify against %q", h)
	}
	if verifyPassword("wrong password", h) {
		t.Fatal("a wrong password must not verify")
	}
	// Malformed / empty verifiers never match.
	for _, bad := range []string{"", "garbage", "pbkdf2$abc$x$y", "pbkdf2$1000$@@@$%%%"} {
		if verifyPassword(pw, bad) {
			t.Fatalf("malformed verifier %q must not match", bad)
		}
	}
	// A fresh hash uses a random salt, so two hashes of the same password differ.
	h2, _ := hashPassword(pw)
	if h == h2 {
		t.Fatal("expected per-hash random salt to make verifiers differ")
	}
}
