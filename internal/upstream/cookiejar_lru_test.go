package upstream

import (
	"testing"
	"time"
)

func TestJarLRUReusesSameJar(t *testing.T) {
	l := newJarLRU(8)
	a := l.getOrCreate("acct:eg:host")
	b := l.getOrCreate("acct:eg:host")
	if a != b {
		t.Fatalf("expected the same jar instance for the same key")
	}
	if l.len() != 1 {
		t.Fatalf("expected 1 jar, got %d", l.len())
	}
}

func TestJarLRUEvictsLeastRecentlyUsed(t *testing.T) {
	l := newJarLRU(3)
	k1, k2, k3 := "k1", "k2", "k3"
	j1 := l.getOrCreate(k1)
	l.getOrCreate(k2)
	l.getOrCreate(k3)
	if l.len() != 3 {
		t.Fatalf("expected 3 jars at capacity, got %d", l.len())
	}
	// Touch k1 so k2 becomes the least-recently-used, then overflow.
	if again := l.getOrCreate(k1); again != j1 {
		t.Fatalf("touching k1 should return the existing jar")
	}
	l.getOrCreate("k4") // exceeds max=3 → evict LRU (k2)
	if l.len() != 3 {
		t.Fatalf("expected len to stay at max=3, got %d", l.len())
	}
	// k2 was evicted: a fresh getOrCreate must return a NEW jar (not the old one).
	if _, ok := l.items["k2"]; ok {
		t.Fatalf("k2 should have been evicted as the LRU entry")
	}
	// k1 must have survived (it was recently touched).
	if _, ok := l.items[k1]; !ok {
		t.Fatalf("k1 should have survived eviction")
	}
}

func TestJarLRUDefaultMax(t *testing.T) {
	if l := newJarLRU(0); l.max != defaultJarLRUMax {
		t.Fatalf("expected default max %d, got %d", defaultJarLRUMax, l.max)
	}
}

func TestJarLRUExpiresIdleJar(t *testing.T) {
	l := newJarLRU(8)
	now := time.Unix(100, 0)
	l.now = func() time.Time { return now }
	l.ttl = time.Hour
	first := l.getOrCreate("key")
	now = now.Add(time.Hour)
	if next := l.getOrCreate("key"); next == first {
		t.Fatal("expired jar was reused")
	}
}
