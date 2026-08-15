package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

// TestConcurrentMultiCLIRootChildAffinityUnderNewRequestPressure models several
// downstream Codex processes at once. Each process keeps a root lease open while
// four child agents and unrelated new roots arrive concurrently. Child traffic is
// deliberately routed while its parent account is busier than the alternatives:
// the only acceptable reason to keep that account is the canonical parent-thread
// affinity, which preserves upstream context and prompt-cache locality.
func TestConcurrentMultiCLIRootChildAffinityUnderNewRequestPressure(t *testing.T) {
	const (
		group               = "multi-cli-affinity-stress"
		model               = "gpt-5.6-sol"
		accountCount        = 6
		cliCount            = 12
		childrenPerCLI      = 4
		independentRequests = 48
	)
	ctx := context.Background()
	store := testStore(t)
	for index := 0; index < accountCount; index++ {
		accountID := fmt.Sprintf("multi-cli-account-%02d", index)
		if err := store.UpsertAccount(ctx, storage.Account{
			ID: accountID, Label: accountID, GroupName: group,
			Provider: "codex", Status: "active", CreatedAt: int64(index + 1),
		}, storage.AccountToken{AccessToken: "test-only-" + accountID}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
			AccountID: accountID, ModelSlug: model, NativeMaxContextWindow: 1_000_000,
			Source: "multi-cli-stress-fixture", LastProbeAt: storage.Now(),
		}}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
			AccountID: accountID, Provider: "codex", Model: model,
			LimiterType: "tokens", Source: "multi-cli-stress-fixture",
			UsedPercent: 1, LimitTokens: 10_000_000, RemainingTokens: 9_900_000,
			LimitRequests: 100_000, RemainingRequests: 99_000,
			ResetAt: storage.Now() + 3600, Status: "allowed",
		}); err != nil {
			t.Fatal(err)
		}
	}

	s := New(store, config.Default())
	type rootLease struct {
		thread   string
		affinity routing.AffinityKey
		lease    Lease
	}
	roots := make([]rootLease, 0, cliCount)
	for index := 0; index < cliCount; index++ {
		thread := fmt.Sprintf("downstream-cli-%02d-root", index)
		request, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Thread-Id", thread)
		affinity := routing.ExtractAffinityKey(request, []byte(fmt.Sprintf(`{"prompt_cache_key":%q}`, thread)))
		lease, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model, Affinity: affinity})
		if err != nil {
			t.Fatalf("select root %d: %v", index, err)
		}
		roots = append(roots, rootLease{thread: thread, affinity: affinity, lease: lease})
	}
	defer func() {
		for index := range roots {
			roots[index].lease.Release()
		}
	}()

	start := make(chan struct{})
	var groupWait sync.WaitGroup
	failures := make(chan error, cliCount*childrenPerCLI+independentRequests)
	independentAccounts := make(chan string, independentRequests)
	for rootIndex := range roots {
		root := &roots[rootIndex]
		for childIndex := 0; childIndex < childrenPerCLI; childIndex++ {
			childIndex := childIndex
			groupWait.Add(1)
			go func() {
				defer groupWait.Done()
				<-start
				request, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
				if err != nil {
					failures <- err
					return
				}
				request.Header.Set("Thread-Id", fmt.Sprintf("%s-child-%02d", root.thread, childIndex))
				request.Header.Set("X-Codex-Parent-Thread-Id", root.thread)
				childAffinity := routing.ExtractAffinityKey(request, []byte(`{"input":"child"}`))
				if childAffinity.Hash != root.affinity.Hash || childAffinity.Source != routing.CodexRootThreadAffinitySource {
					failures <- fmt.Errorf("child affinity drift root=%+v child=%+v", root.affinity, childAffinity)
					return
				}
				lease, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model, Affinity: childAffinity})
				if err != nil {
					failures <- fmt.Errorf("select child for %s: %w", root.thread, err)
					return
				}
				defer lease.Release()
				if lease.Account.ID != root.lease.Account.ID {
					failures <- fmt.Errorf("root %s account=%s child=%d account=%s", root.thread, root.lease.Account.ID, childIndex, lease.Account.ID)
				}
			}()
		}
	}
	for index := 0; index < independentRequests; index++ {
		groupWait.Add(1)
		go func(index int) {
			defer groupWait.Done()
			<-start
			request, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
			if err != nil {
				failures <- err
				return
			}
			thread := fmt.Sprintf("new-independent-cli-%02d", index)
			request.Header.Set("Thread-Id", thread)
			affinity := routing.ExtractAffinityKey(request, []byte(fmt.Sprintf(`{"prompt_cache_key":%q}`, thread)))
			lease, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model, Affinity: affinity})
			if err != nil {
				failures <- fmt.Errorf("select independent %d: %w", index, err)
				return
			}
			defer lease.Release()
			independentAccounts <- lease.Account.ID
		}(index)
	}
	close(start)
	groupWait.Wait()
	close(failures)
	close(independentAccounts)
	for failure := range failures {
		t.Error(failure)
	}
	used := make(map[string]int)
	for accountID := range independentAccounts {
		used[accountID]++
	}
	if len(used) < 2 {
		t.Fatalf("new requests were not pressure-balanced across the pool: %v", used)
	}
	t.Logf("MULTI_CLI_AFFINITY roots=%d children=%d independent=%d accounts_used=%d", cliCount, cliCount*childrenPerCLI, independentRequests, len(used))
}
