package storage

import (
	"context"
	"fmt"
	"testing"
)

// TestTouchContextJournalSlidesExpiryAndRefusesResurrection verifies the sliding-TTL
// primitive behind arbitrary-duration /goal resume: a live row's expiry moves forward on
// touch, but an already-expired row is never brought back.
func TestTouchContextJournalSlidesExpiryAndRefusesResurrection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := Now()

	if err := s.PutContextJournal(ctx, ContextJournal{ResponseID: "r1", Payload: "{}", ExpiresAt: now + 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchContextJournal(ctx, "r1", now+100000); err != nil {
		t.Fatal(err)
	}
	j, err := s.GetContextJournal(ctx, "r1")
	if err != nil {
		t.Fatalf("live row should still be readable after touch: %v", err)
	}
	if j.ExpiresAt != now+100000 {
		t.Fatalf("expiry was not slid forward: got %d want %d", j.ExpiresAt, now+100000)
	}

	if err := s.PutContextJournal(ctx, ContextJournal{ResponseID: "r2", Payload: "{}", ExpiresAt: now - 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchContextJournal(ctx, "r2", now+100000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetContextJournal(ctx, "r2"); err == nil {
		t.Fatal("an already-expired row must not be resurrected by a touch")
	}
}

// TestEvictContextJournalToBudgetEvictsLowestExpiryFirst verifies the low-VPS disk bound:
// the least-recently-resumed chains (lowest expires_at) are evicted first, and the most
// active ones (highest expires_at, kept warm by sliding TTL) survive.
func TestEvictContextJournalToBudgetEvictsLowestExpiryFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := Now()
	for i := 0; i < 5; i++ {
		if err := s.PutContextJournal(ctx, ContextJournal{ResponseID: fmt.Sprintf("r%d", i), Payload: "{}", ExpiresAt: now + int64(100+i)}); err != nil {
			t.Fatal(err)
		}
	}
	evicted, err := s.EvictContextJournalToBudget(ctx, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 2 {
		t.Fatalf("expected 2 rows evicted to reach maxRows=3, got %d", evicted)
	}
	if _, err := s.GetContextJournal(ctx, "r0"); err == nil {
		t.Fatal("r0 (lowest expiry = least recently resumed) should be evicted")
	}
	if _, err := s.GetContextJournal(ctx, "r4"); err != nil {
		t.Fatalf("r4 (highest expiry = most active) should be kept: %v", err)
	}
	if n, _ := s.EvictContextJournalToBudget(ctx, 0, 0); n != 0 {
		t.Fatalf("a disabled budget must evict nothing, got %d", n)
	}
}
