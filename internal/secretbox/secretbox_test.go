package secretbox

import "testing"

func TestSealOpenRoundTrip(t *testing.T) {
	key := DeriveKey([]byte("a-deployment-secret"))
	for _, pt := range []string{"access-token-xyz", "a", "日本語トークン", ""} {
		sealed, err := Seal(key, pt)
		if err != nil {
			t.Fatalf("Seal(%q): %v", pt, err)
		}
		if pt != "" && !IsSealed(sealed) {
			t.Fatalf("Seal(%q) not marked sealed: %q", pt, sealed)
		}
		if pt == "" && sealed != "" {
			t.Fatalf("Seal(\"\") should stay empty, got %q", sealed)
		}
		got, err := Open(key, sealed)
		if err != nil {
			t.Fatalf("Open(%q): %v", sealed, err)
		}
		if got != pt {
			t.Fatalf("round trip = %q, want %q", got, pt)
		}
	}
}

func TestOpenLegacyPlaintextPassthrough(t *testing.T) {
	key := DeriveKey([]byte("k"))
	got, err := Open(key, "legacy-plaintext-token")
	if err != nil || got != "legacy-plaintext-token" {
		t.Fatalf("legacy passthrough = %q,%v; want unchanged", got, err)
	}
}

func TestEncryptionWithoutKeyFailsClosed(t *testing.T) {
	if sealed, err := Seal(nil, "tok"); err == nil || sealed != "" {
		t.Fatalf("Seal without a key = %q,%v; want empty,error", sealed, err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	sealed, _ := Seal(DeriveKey([]byte("key-A")), "secret")
	if _, err := Open(DeriveKey([]byte("key-B")), sealed); err == nil {
		t.Fatal("Open with wrong key should fail, got nil error")
	}
}

func TestNonDeterministic(t *testing.T) {
	key := DeriveKey([]byte("k"))
	a, _ := Seal(key, "same")
	b, _ := Seal(key, "same")
	if a == b {
		t.Fatal("two seals of the same plaintext should differ (random nonce)")
	}
}

func TestDomainIsolation(t *testing.T) {
	key := DeriveKey([]byte("k"))
	sealed, err := SealDomain(key, "tokens", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDomain(key, "cookies", sealed); err == nil {
		t.Fatal("ciphertext opened in a different domain")
	}
	if got, err := OpenDomain(key, "tokens", sealed); err != nil || got != "secret" {
		t.Fatalf("same-domain open = %q,%v", got, err)
	}
}
