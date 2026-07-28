package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestUserGroupTargetBindingClaimAndCompareAndSwap(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const contenders = 32
	if err := store.CreateUserGroupDefinition(ctx, UserGroup{
		ID: "claim-group", Name: "Claim Group",
		Targets: []TargetRef{{Kind: TargetKindAccountPoolGroup, ID: "cyber"}},
	}); err != nil {
		t.Fatalf("create user group fixture: %v", err)
	}

	start := make(chan struct{})
	results := make(chan UserGroupTargetBinding, contenders)
	claimed := make(chan bool, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			actual, won, err := store.ClaimUserGroupTargetBinding(ctx, UserGroupTargetBinding{
				UserGroupID: "claim-group", AffinityKey: "conversation-hash",
				Target: TargetRef{Kind: TargetKindAccountPoolGroup, ID: fmt.Sprintf("pool-%02d", i)},
			})
			if err != nil {
				errs <- err
				return
			}
			results <- actual
			claimed <- won
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(claimed)
	close(errs)
	for err := range errs {
		t.Fatalf("claim: %v", err)
	}

	winners := 0
	winnerID := ""
	for won := range claimed {
		if won {
			winners++
		}
	}
	for actual := range results {
		if winnerID == "" {
			winnerID = actual.Target.ID
		}
		if actual.Target.ID != winnerID {
			t.Fatalf("claims observed different winners: first=%q current=%q", winnerID, actual.Target.ID)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners=%d, want 1", winners)
	}

	winner := TargetRef{Kind: TargetKindAccountPoolGroup, ID: winnerID}
	replacement := UserGroupTargetBinding{
		UserGroupID: "claim-group", AffinityKey: "conversation-hash",
		Target: TargetRef{Kind: TargetKindAccountPoolGroup, ID: "migrated-pool"},
	}
	actual, swapped, err := store.CompareAndSwapUserGroupTargetBinding(ctx, winner, replacement)
	if err != nil || !swapped || actual.Target != replacement.Target {
		t.Fatalf("CAS actual=%+v swapped=%v err=%v", actual, swapped, err)
	}
	staleReplacement := replacement
	staleReplacement.Target.ID = "stale-loser"
	actual, swapped, err = store.CompareAndSwapUserGroupTargetBinding(ctx, winner, staleReplacement)
	if err != nil || swapped || actual.Target != replacement.Target {
		t.Fatalf("stale CAS actual=%+v swapped=%v err=%v", actual, swapped, err)
	}
}
